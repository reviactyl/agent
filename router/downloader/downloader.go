package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/google/uuid"

	"github.com/reviactyl/agent/config"
	wnet "github.com/reviactyl/agent/internal/network"
	"github.com/reviactyl/agent/server"
	"github.com/reviactyl/agent/server/filesystem"
)

var client *http.Client

func init() {
	dialer := &net.Dialer{
		LocalAddr: nil,
		Timeout:   time.Second * 30,
		// Check every address immediately before connecting to it, since the
		// destination is resolved again when dialing.
		ControlContext: checkConnection,
	}

	trnspt := http.DefaultTransport.(*http.Transport).Clone()
	// Always connect directly, so that every destination is checked.
	trnspt.Proxy = nil
	// Store the file exactly as it is served. Asking for a compressed response
	// would also hide the length of the file, which is needed to write it.
	trnspt.DisableCompression = true
	trnspt.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		// Refuse the destination if any of its addresses are internal, or if it
		// cannot be resolved.
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil || len(ips) == 0 {
			log.WithField("host", host).WithField("error", err).Debug("downloader: failed to resolve remote file host")
			return nil, errors.WithStack(ErrInvalidIPAddress)
		}
		for _, ip := range ips {
			if isBlocked(host, ip) {
				return nil, errors.WithStack(ErrInternalResolution)
			}
		}
		return dialer.DialContext(context.WithValue(ctx, dialHostKey{}, host), network, addr)
	}

	client = &http.Client{
		Timeout:   time.Hour * 2,
		Transport: trnspt,
		// Disallow any redirect on an HTTP call. This is a security requirement: do not modify
		// this logic without first ensuring that the new target location IS NOT within the current
		// instance's local network.
		//
		// This specific error response just causes the client to not follow the redirect and
		// returns the actual redirect response to the caller. Not perfect, but simple and most
		// people won't be using URLs that redirect anyways hopefully?
		//
		// We'll re-evaluate this down the road if needed.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// dialHostKey is the context key for the host name a connection is being made
// to, which is checked against the allowlist when the connection is made.
type dialHostKey struct{}

// checkConnection is called with the address a connection is about to be made
// to, and refuses to connect to any internal address.
func checkConnection(ctx context.Context, _ string, address string, _ syscall.RawConn) error {
	ipStr, _, err := net.SplitHostPort(address)
	if err != nil {
		return errors.WithStack(err)
	}
	ip, err := netip.ParseAddr(ipStr)
	if err != nil {
		return errors.WithStack(ErrInternalResolution)
	}
	host, _ := ctx.Value(dialHostKey{}).(string)
	if isBlocked(host, ip) {
		return errors.WithStack(ErrInternalResolution)
	}
	return nil
}

// isBlocked reports whether connecting to the address, resolved from the host,
// is refused because it is internal and not allowed by the operator.
func isBlocked(host string, ip netip.Addr) bool {
	return wnet.IsInternal(ip) && !wnet.IsAllowed(config.Get().Api.RemoteDownloadAllowlist, host, ip)
}

var instance = &Downloader{
	// Tracks all the active downloads.
	downloadCache: make(map[string]*Download),
	// Tracks all the downloads active for a given server instance. This is
	// primarily used to make things quicker and keep the code a little more
	// legible throughout here.
	serverCache: make(map[string][]string),
}

const (
	ErrInternalResolution = errors.Sentinel("downloader: destination resolves to internal network location")
	ErrInvalidIPAddress   = errors.Sentinel("downloader: invalid IP address")
	ErrDownloadFailed     = errors.Sentinel("downloader: download request failed")
)

type Counter struct {
	total   int
	onWrite func(total int)
}

func (c *Counter) Write(p []byte) (int, error) {
	n := len(p)
	c.total += n
	c.onWrite(c.total)
	return n, nil
}

type DownloadRequest struct {
	Directory     string
	URL           *url.URL
	FileName      string
	UseHeader     bool
	RetainFailure bool
}

type Download struct {
	Identifier string
	path       string
	mu         sync.RWMutex
	req        DownloadRequest
	server     *server.Server
	progress   float64
	cancelFunc context.CancelFunc
	canceled   bool
	failure    string
}

// New starts a new tracked download which allows for cancellation later on by calling
// Download.Cancel. It returns nil if the server already has three downloads.
func New(s *server.Server, r DownloadRequest) *Download {
	dl := Download{
		Identifier: uuid.Must(uuid.NewRandom()).String(),
		req:        r,
		server:     s,
	}
	if !instance.track(&dl) {
		return nil
	}
	return &dl
}

// ByServer returns all the tracked downloads for a given server instance.
func ByServer(sid string) []*Download {
	instance.mu.Lock()
	defer instance.mu.Unlock()
	var downloads []*Download
	if v, ok := instance.serverCache[sid]; ok {
		for _, id := range v {
			if dl, ok := instance.downloadCache[id]; ok {
				downloads = append(downloads, dl)
			}
		}
	}
	for _, failure := range instance.failures {
		if failure.download.server.ID() == sid {
			downloads = append(downloads, failure.download)
		}
	}
	return downloads
}

// ByID returns a single Download matching a given identifier. If no download is found
// the second argument in the response will be false.
func ByID(dlid string) *Download {
	return instance.find(dlid)
}

func (dl *Download) MarshalJSON() ([]byte, error) {
	dl.mu.RLock()
	defer dl.mu.RUnlock()
	status := "downloading"
	if dl.failure != "" {
		status = "failed"
	}
	return json.Marshal(struct {
		Identifier string
		Progress   float64
		Status     string
		Error      string `json:",omitempty"`
	}{dl.Identifier, dl.progress, status, dl.failure})
}

// Execute executes a given download for the server and begins writing the file to the disk. Once
// successful the download is removed; requested failure history is retained temporarily.
func (dl *Download) Execute() (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour*12)
	dl.mu.Lock()
	if dl.canceled {
		dl.mu.Unlock()
		cancel()
		return context.Canceled
	}
	dl.cancelFunc = cancel
	dl.mu.Unlock()
	defer func() {
		cancel()
		instance.complete(dl, err)
	}()

	// At this point we have verified the destination is not within the local network, so we can
	// now make a request to that URL and pull down the file, saving it to the server's data
	// directory.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dl.req.URL.String(), nil)
	if err != nil {
		return errors.WrapIf(err, "downloader: failed to create request")
	}

	req.Header.Set("User-Agent", "Reviactyl Panel (https://reviactyl.app)")
	res, err := client.Do(req)
	if err != nil {
		if IsDownloadError(err) {
			return err
		}
		return errors.WithStack(fmt.Errorf("%w: %w", ErrDownloadFailed, err))
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return &responseStatusError{code: res.StatusCode}
	}

	if dl.req.UseHeader {
		if contentDisposition := res.Header.Get("Content-Disposition"); contentDisposition != "" {
			_, params, err := mime.ParseMediaType(contentDisposition)
			if err != nil {
				return errors.WrapIf(err, "downloader: invalid \"Content-Disposition\" header")
			}

			// Only use the name of the file, so that it is saved in the
			// directory that was asked for.
			if v, ok := params["filename"]; ok {
				if name := filepath.Base(filepath.Clean("/" + v)); name != "/" && name != "." {
					dl.path = name
				}
			}
		}
	}
	if dl.path == "" {
		if dl.req.FileName != "" {
			dl.path = dl.req.FileName
		} else {
			parts := strings.Split(dl.req.URL.Path, "/")
			dl.path = parts[len(parts)-1]
		}
	}

	p := dl.Path()
	if err := dl.server.Filesystem().IsIgnored(p); err != nil {
		return err
	}
	dl.server.Log().WithField("path", p).Debug("writing remote file to disk")

	// Write the file while tracking the progress, Write will check that the
	// size of the file won't exceed the disk limit.
	r := io.TeeReader(res.Body, dl.counter(res.ContentLength))
	if err := dl.server.Filesystem().Write(p, r, res.ContentLength, 0o644); err != nil {
		return errors.WrapIf(err, "downloader: failed to write file to server directory")
	}
	dl.mu.Lock()
	dl.progress = 1
	dl.mu.Unlock()
	return nil
}

// Cancel cancels a running download and frees up the associated resources. If a file is being
// written a partial file will remain present on the disk.
func (dl *Download) Cancel() {
	dl.mu.Lock()
	dl.canceled = true
	cancel := dl.cancelFunc
	dl.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	instance.remove(dl.Identifier)
}

// BelongsTo checks if the given download belongs to the provided server.
func (dl *Download) BelongsTo(s *server.Server) bool {
	return dl.server.ID() == s.ID()
}

// Progress returns the current progress of the download as a float value between 0 and 1 where
// 1 indicates that the download is completed.
func (dl *Download) Progress() float64 {
	dl.mu.RLock()
	defer dl.mu.RUnlock()
	return dl.progress
}

func (dl *Download) Path() string {
	return filepath.Join(dl.req.Directory, dl.path)
}

// Handles a write event by updating the progress completed percentage and firing off
// events to the server websocket as needed.
func (dl *Download) counter(contentLength int64) *Counter {
	onWrite := func(t int) {
		dl.mu.Lock()
		defer dl.mu.Unlock()
		if contentLength > 0 {
			dl.progress = float64(t) / float64(contentLength)
		}
	}
	return &Counter{
		onWrite: onWrite,
	}
}

// Downloader represents a global downloader that keeps track of all currently processing downloads
// for the machine.
type Downloader struct {
	mu            sync.RWMutex
	downloadCache map[string]*Download
	serverCache   map[string][]string
	failures      map[string]*failedDownload
}

// track tracks a download in the internal cache for this instance.
func (d *Downloader) track(dl *Download) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	sid := dl.server.ID()
	if len(d.serverCache[sid]) >= 3 {
		return false
	}
	if _, ok := d.downloadCache[dl.Identifier]; !ok {
		d.downloadCache[dl.Identifier] = dl
		if _, ok := d.serverCache[sid]; !ok {
			d.serverCache[sid] = []string{}
		}
		d.serverCache[sid] = append(d.serverCache[sid], dl.Identifier)
	}
	return true
}

// find finds a given download entry using the provided ID and returns it.
func (d *Downloader) find(dlid string) *Download {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if entry, ok := d.downloadCache[dlid]; ok {
		return entry
	}
	if entry, ok := d.failures[dlid]; ok {
		return entry.download
	}
	return nil
}

// remove removes the given download reference from the cache storing them. This also updates
// the slice of active downloads for a given server to not include this download.
func (d *Downloader) remove(dlID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.removeLocked(dlID)
}

func (d *Downloader) removeLocked(dlID string) {
	if failure, ok := d.failures[dlID]; ok {
		failure.timer.Stop()
		delete(d.failures, dlID)
	}
	if _, ok := d.downloadCache[dlID]; !ok {
		return
	}
	sID := d.downloadCache[dlID].server.ID()
	delete(d.downloadCache, dlID)
	if tracked, ok := d.serverCache[sID]; ok {
		var out []string
		for _, k := range tracked {
			if k != dlID {
				out = append(out, k)
			}
		}
		if len(out) == 0 {
			delete(d.serverCache, sID)
		} else {
			d.serverCache[sID] = out
		}
	}
}

func IsDownloadError(err error) bool {
	return errors.Is(err, ErrDownloadFailed) || errors.Is(err, ErrInvalidIPAddress) || errors.Is(err, ErrInternalResolution)
}

const failureRetention = 15 * time.Minute
const maxRetainedFailures = 100

type failedDownload struct {
	download *Download
	finished time.Time
	timer    *time.Timer
}

func (d *Downloader) complete(dl *Download, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	dl.mu.Lock()
	defer dl.mu.Unlock()
	d.removeLocked(dl.Identifier)
	dl.cancelFunc = nil
	canceled := dl.canceled
	dl.canceled = true
	if err == nil || canceled || !dl.req.RetainFailure {
		return
	}
	dl.failure = failureMessage(err)
	if d.failures == nil {
		d.failures = make(map[string]*failedDownload)
	}
	if len(d.failures) >= maxRetainedFailures {
		var oldestID string
		var oldest time.Time
		for id, failure := range d.failures {
			if oldestID == "" || failure.finished.Before(oldest) {
				oldestID, oldest = id, failure.finished
			}
		}
		d.removeLocked(oldestID)
	}
	failure := &failedDownload{download: dl, finished: time.Now()}
	failure.timer = time.AfterFunc(failureRetention, func() { d.remove(dl.Identifier) })
	d.failures[dl.Identifier] = failure
}

type responseStatusError struct{ code int }

func (e *responseStatusError) Error() string {
	return fmt.Sprintf("downloader: got bad response status from endpoint: %d %s", e.code, http.StatusText(e.code))
}

func failureMessage(err error) string {
	var status *responseStatusError
	switch {
	case errors.As(err, &status):
		return fmt.Sprintf("The remote server returned HTTP %d %s.", status.code, http.StatusText(status.code))
	case errors.Is(err, context.DeadlineExceeded):
		return "The remote download timed out. Please try again."
	case IsDownloadError(err):
		return "The remote file could not be downloaded. Check that the URL is reachable and allowed."
	case filesystem.IsErrorCode(err, filesystem.ErrCodeDiskSpace), errors.Is(err, syscall.ENOSPC):
		return "There is not enough disk space to download this file."
	case filesystem.IsErrorCode(err, filesystem.ErrCodeIsDirectory), errors.Is(err, syscall.EISDIR):
		return "The download destination is an existing directory. Choose a different filename."
	case filesystem.IsErrorCode(err, filesystem.ErrCodeDenylistFile), filesystem.IsErrorCode(err, filesystem.ErrCodePathResolution):
		return "Writing to the download destination is prohibited."
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.ErrShortWrite):
		return "The remote file was incomplete and could not be saved."
	case errors.Is(err, os.ErrPermission):
		return "The download destination is not writable. Check its file permissions."
	default:
		return "The remote file could not be saved to the server directory. Check the destination and available disk space."
	}
}
