package router

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/apex/log"
	"github.com/gin-gonic/gin"
	"github.com/reviactyl/agent/router/tokens"

	"github.com/reviactyl/agent/config"
	"github.com/reviactyl/agent/router/middleware"
	"github.com/reviactyl/agent/server"
	"github.com/reviactyl/agent/server/installer"
	"github.com/reviactyl/agent/system"
)

// Returns information about the system that agent is running on.
var readSystemInformation = system.GetSystemInformation

func getSystemInformation(c *gin.Context) {
	i, err := readSystemInformation()
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	if c.Query("v") == "2" {
		c.JSON(http.StatusOK, i)
		return
	}

	c.JSON(http.StatusOK, struct {
		Architecture     string `json:"architecture"`
		CPUCount         int    `json:"cpu_count"`
		KernelVersion    string `json:"kernel_version"`
		OS               string `json:"os"`
		Version          string `json:"version"`
		InstallationType string `json:"installation_type"`
	}{
		Architecture:     i.System.Architecture,
		CPUCount:         i.System.CPUThreads,
		KernelVersion:    i.System.KernelVersion,
		OS:               i.System.OSType,
		Version:          i.Version,
		InstallationType: i.InstallationType,
	})
}

type postSystemUpdateRequest struct {
	Version string `json:"version" binding:"required"`
	Channel string `json:"channel" binding:"omitempty,oneof=stable beta"`
}

type postSystemUpdateResponse struct {
	Version string `json:"version"`
	Status  string `json:"status"`
}

var installSystemUpdate = func(ctx context.Context, version string, channel string) (*system.InstalledUpdate, error) {
	return system.NewUpdater().Install(ctx, version, channel)
}

var restartAfterSystemUpdate = system.RestartAfterUpdate

var systemUpdateInProgress atomic.Bool

var systemUpdateTimeout = 2 * time.Minute

var systemUpdateRestartTimeout = 20 * time.Second

var systemUpdateLockRelease = 5 * time.Minute

var scheduleSystemUpdateLockRelease = time.AfterFunc

func postSystemUpdate(c *gin.Context) {
	if system.InstallationType != "native" {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "automatic updates are only available for native Agent installations",
		})
		return
	}
	var request postSystemUpdateRequest
	if err := c.BindJSON(&request); err != nil {
		return
	}

	if !systemUpdateInProgress.CompareAndSwap(false, true) {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "an Agent update is already in progress",
		})
		return
	}

	operationContext := context.Background()
	ctx, cancel := context.WithTimeout(operationContext, systemUpdateTimeout)
	defer cancel()
	installed, err := installSystemUpdate(ctx, request.Version, request.Channel)
	if err != nil {
		systemUpdateInProgress.Store(false)
		if errors.Is(err, system.ErrInvalidUpdateRequest) {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		middleware.CaptureAndAbort(c, err)
		return
	}

	restartContext, restartCancel := context.WithTimeout(operationContext, systemUpdateRestartTimeout)
	defer restartCancel()
	rollbackSafe, err := restartAfterSystemUpdate(restartContext, installed)
	if err != nil {
		if rollbackSafe {
			if rollbackErr := system.RollbackInstalledUpdate(installed); rollbackErr != nil {
				err = fmt.Errorf("%w (rollback failed: %v)", err, rollbackErr)
			}
			systemUpdateInProgress.Store(false)
		}
		middleware.CaptureAndAbort(c, err)
		return
	}

	// The transient supervisor should replace this process almost immediately.
	// Release the guard if that never happens so a failed restart cannot block
	// future update attempts until the Agent is restarted manually.
	scheduleSystemUpdateLockRelease(systemUpdateLockRelease, func() {
		systemUpdateInProgress.Store(false)
	})

	c.JSON(http.StatusAccepted, postSystemUpdateResponse{
		Version: request.Version,
		Status:  "restarting",
	})
}

// Returns all the servers that are registered and configured correctly on
// this agent instance.
func getAllServers(c *gin.Context) {
	servers := middleware.ExtractManager(c).All()
	out := make([]server.APIResponse, len(servers), len(servers))
	for i, v := range servers {
		out[i] = v.ToAPIResponse()
	}
	c.JSON(http.StatusOK, out)
}

// Creates a new server on the agent daemon and begins the installation process
// for it.
func postCreateServer(c *gin.Context) {
	manager := middleware.ExtractManager(c)

	details := installer.ServerDetails{}
	if err := c.BindJSON(&details); err != nil {
		return
	}

	install, err := installer.New(c.Request.Context(), manager, details)
	if err != nil {
		if installer.IsValidationError(err) {
			c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{
				"error": "The data provided in the request could not be validated.",
			})
			return
		}

		middleware.CaptureAndAbort(c, err)
		return
	}

	// Plop that server instance onto the request so that it can be referenced in
	// requests from here-on out.
	manager.Add(install.Server())

	// Begin the installation process in the background to not block the request
	// cycle. If there are any errors they will be logged and communicated back
	// to the Panel where a reinstall may take place.
	go func(i *installer.Installer) {
		if err := i.Server().CreateEnvironment(); err != nil {
			i.Server().Log().WithField("error", err).Error("failed to create server environment during install process")
			return
		}

		if err := i.Server().Install(); err != nil {
			log.WithFields(log.Fields{"server": i.Server().ID(), "error": err}).Error("failed to run install process for server")
			return
		}

		if i.StartOnCompletion {
			log.WithField("server_id", i.Server().ID()).Debug("starting server after successful installation")
			if err := i.Server().HandlePowerAction(server.PowerActionStart, 30); err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					log.WithFields(log.Fields{"server_id": i.Server().ID(), "action": "start"}).Warn("could not acquire a lock while attempting to perform a power action")
				} else {
					log.WithFields(log.Fields{"server_id": i.Server().ID(), "action": "start", "error": err}).Error("encountered error processing a server power action in the background")
				}
			}
		} else {
			log.WithField("server_id", i.Server().ID()).Debug("skipping automatic start after successful server installation")
		}
	}(install)

	c.Status(http.StatusAccepted)
}

type postUpdateConfigurationResponse struct {
	Applied bool `json:"applied"`
}

// Updates the running configuration for this Agent instance.
func postUpdateConfiguration(c *gin.Context) {
	cfg := config.Get()

	if cfg.IgnorePanelConfigUpdates {
		c.JSON(http.StatusOK, postUpdateConfigurationResponse{
			Applied: false,
		})
		return
	}

	if err := c.BindJSON(&cfg); err != nil {
		return
	}

	// Keep the SSL certificates the same since the Panel will send through Lets Encrypt
	// default locations. However, if we picked a different location manually we don't
	// want to override that.
	//
	// If you pass through manual locations in the API call this logic will be skipped.
	if strings.HasPrefix(cfg.Api.Ssl.KeyFile, "/etc/letsencrypt/live/") {
		cfg.Api.Ssl.KeyFile = config.Get().Api.Ssl.KeyFile
		cfg.Api.Ssl.CertificateFile = config.Get().Api.Ssl.CertificateFile
	}

	// The token that everything authenticates against is a derived value that is
	// not part of the payload sent by the Panel, so it has to be re-resolved from
	// the new token values.
	if err := cfg.ResolveToken(true); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	// Refuse to go any further with a token we could never authenticate against.
	if cfg.Token.ID == "" || cfg.Token.Token == "" {
		middleware.CaptureAndAbort(c, errors.New("config: refusing to apply an update with an empty authentication token"))
		return
	}

	tokenId, token := cfg.Token.ID, cfg.Token.Token

	// Try to write this new configuration to the disk before updating our global
	// state with it.
	if err := config.WriteToDisk(cfg); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	// Since we wrote it to the disk successfully now update the global configuration
	// state to use this new configuration struct.
	config.Set(cfg)

	// Requests we make back to the Panel use credentials that were captured when
	// the client was created at boot, so they have to be rotated explicitly.
	middleware.ExtractManager(c).Client().SetCredentials(tokenId, token)

	c.JSON(http.StatusOK, postUpdateConfigurationResponse{
		Applied: true,
	})
}

func postDeauthorizeUser(c *gin.Context) {
	var data struct {
		User    string   `json:"user"`
		Servers []string `json:"servers"`
	}

	if err := c.BindJSON(&data); err != nil {
		return
	}

	// todo: disconnect websockets more gracefully
	m := middleware.ExtractManager(c)
	if len(data.Servers) > 0 {
		for _, uuid := range data.Servers {
			if s, ok := m.Get(uuid); ok {
				tokens.DenyForServer(s.ID(), data.User)
				s.Websockets().CancelAll()
				s.Sftp().Cancel(data.User)
			}
		}
	} else {
		for _, s := range m.All() {
			tokens.DenyForServer(s.ID(), data.User)
			s.Websockets().CancelAll()
			s.Sftp().Cancel(data.User)
		}
	}

	c.Status(http.StatusNoContent)
}
