package docker

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/stretchr/testify/require"

	"github.com/reviactyl/agent/environment"
	"github.com/reviactyl/agent/events"
	"github.com/reviactyl/agent/remote"
	"github.com/reviactyl/agent/system"
)

// attachedEnvironment returns an environment attached to one end of a pipe,
// returning the other end, which receives the process input.
func attachedEnvironment(t *testing.T) (*Environment, net.Conn) {
	t.Helper()
	local, remote := net.Pipe()
	t.Cleanup(func() {
		_ = local.Close()
		_ = remote.Close()
	})
	e := &Environment{meta: &Metadata{}, st: system.NewAtomicString(environment.ProcessRunningState)}
	e.SetStream(&types.HijackedResponse{Conn: local})
	return e, remote
}

// Commands may not contain control characters, other than tabs.
func TestSendCommandRejectsControlCharacters(t *testing.T) {
	e, input := attachedEnvironment(t)
	received := make(chan string, 1)
	go func() {
		b := make([]byte, 64)
		n, _ := input.Read(b)
		received <- string(b[:n])
	}()

	for _, c := range []string{"\x10\x11", "say \x10", "\x03", "\x1b[A", "\x7f"} {
		if err := e.SendCommand(c); err == nil {
			t.Errorf("expected command %q to be rejected", c)
		}
	}
	if err := e.SendCommand("say\thello world"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if got != "say\thello world\n" {
			t.Fatalf("expected only the valid command to be sent, got %q", got)
		}
	case <-time.After(time.Second * 2):
		t.Fatal("expected the valid command to be sent")
	}
}

// Sending a command times out if the process does not read its input.
func TestSendCommandDoesNotBlockForever(t *testing.T) {
	previous := commandWriteTimeout
	commandWriteTimeout = time.Millisecond * 200
	t.Cleanup(func() { commandWriteTimeout = previous })

	e, _ := attachedEnvironment(t)
	done := make(chan error, 1)
	go func() { done <- e.SendCommand("say hello") }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error when the command could not be written")
		}
	case <-time.After(time.Second * 3):
		t.Fatal("expected sending the command to time out")
	}
}

func TestMultilineStopCommandSetsStoppingState(t *testing.T) {
	for _, command := range []string{"stop", "say hi\nstop", "say hi\r\nstop\r\n"} {
		t.Run(command, func(t *testing.T) {
			e, input := attachedEnvironment(t)
			e.emitter = events.NewBus()
			e.meta.Stop = remote.ProcessStopConfiguration{Type: "command", Value: "stop"}
			read := make(chan struct{})
			go func() {
				defer close(read)
				b := make([]byte, 128)
				_, _ = input.Read(b)
			}()
			if err := e.SendCommand(command); err != nil {
				t.Fatal(err)
			}
			<-read
			if e.State() != environment.ProcessStoppingState {
				t.Fatal("stop command did not set the stopping state")
			}
		})
	}
}

func TestStopCommandWriteFailureRestoresState(t *testing.T) {
	for _, method := range []string{"command", "stop"} {
		for _, state := range []string{environment.ProcessRunningState, environment.ProcessStartingState, environment.ProcessStoppingState} {
			t.Run(method+"/"+state, func(t *testing.T) {
				e, input := attachedEnvironment(t)
				e.emitter = events.NewBus()
				e.meta.Stop = remote.ProcessStopConfiguration{Type: "command", Value: "stop"}
				e.SetState(state)
				require.NoError(t, input.Close())
				var err error
				if method == "stop" {
					err = e.Stop(context.Background())
				} else {
					err = e.SendCommand("say goodbye\r\nstop\r\n")
				}
				require.ErrorIs(t, err, io.ErrClosedPipe)
				require.Contains(t, err.Error(), "could not write to container stream")
				require.Equal(t, state, e.State())
			})
		}
	}
}

func TestStopCommandWriteTimeoutRestoresState(t *testing.T) {
	previous := commandWriteTimeout
	commandWriteTimeout = 20 * time.Millisecond
	t.Cleanup(func() { commandWriteTimeout = previous })
	e, _ := attachedEnvironment(t)
	e.emitter = events.NewBus()
	e.meta.Stop = remote.ProcessStopConfiguration{Type: "command", Value: "stop"}
	err := e.SendCommand("stop")
	var netErr net.Error
	require.True(t, errors.As(err, &netErr))
	require.True(t, netErr.Timeout())
	require.Equal(t, environment.ProcessRunningState, e.State())
}

func TestStopCommandWriteSuccess(t *testing.T) {
	for _, method := range []string{"command", "stop"} {
		t.Run(method, func(t *testing.T) {
			e, input := attachedEnvironment(t)
			e.emitter = events.NewBus()
			e.meta.Stop = remote.ProcessStopConfiguration{Type: "command", Value: "stop"}
			done := make(chan error, 1)
			go func() {
				if method == "stop" {
					done <- e.Stop(context.Background())
				} else {
					done <- e.SendCommand("stop")
				}
			}()
			require.NoError(t, input.SetReadDeadline(time.Now().Add(2*time.Second)))
			b := make([]byte, len("stop\n"))
			_, err := io.ReadFull(input, b)
			require.NoError(t, err)
			require.Equal(t, "stop\n", string(b))
			require.Equal(t, environment.ProcessStoppingState, e.State(), "must mark stopping before the process can exit")
			require.NoError(t, <-done)
			require.Equal(t, environment.ProcessStoppingState, e.State())
		})
	}
}

func TestFailedStopCommandPreservesLaterState(t *testing.T) {
	for _, states := range [][]string{
		{environment.ProcessOfflineState},
		{environment.ProcessStartingState},
		{environment.ProcessRunningState},
		{environment.ProcessStoppingState},
		{environment.ProcessOfflineState, environment.ProcessStoppingState},
	} {
		t.Run(strings.Join(states, "/"), func(t *testing.T) {
			e, input := attachedEnvironment(t)
			e.emitter = events.NewBus()
			e.meta.Stop = remote.ProcessStopConfiguration{Type: "command", Value: "stop"}
			changes := make(chan []byte, 8)
			e.Events().On(changes)
			defer e.Events().Off(changes)
			done := make(chan error, 1)
			go func() { done <- e.SendCommand("stop") }()
			select {
			case v := <-changes:
				require.Equal(t, environment.ProcessStoppingState, events.MustDecode(v).Data)
			case <-time.After(2 * time.Second):
				t.Fatal("stop command did not start writing")
			}
			for _, state := range states {
				e.SetState(state)
			}
			require.NoError(t, input.Close())
			require.ErrorIs(t, <-done, io.ErrClosedPipe)
			require.Equal(t, states[len(states)-1], e.State())
		})
	}
}

func TestFailedStopCommandPublishesRestoredState(t *testing.T) {
	e, input := attachedEnvironment(t)
	e.emitter = events.NewBus()
	e.meta.Stop = remote.ProcessStopConfiguration{Type: "command", Value: "stop"}
	changes := make(chan []byte, 8)
	e.Events().On(changes)
	defer e.Events().Off(changes)
	require.NoError(t, input.Close())
	require.ErrorIs(t, e.SendCommand("stop"), io.ErrClosedPipe)
	for _, state := range []string{environment.ProcessStoppingState, environment.ProcessRunningState} {
		select {
		case v := <-changes:
			event := events.MustDecode(v)
			require.Equal(t, environment.StateChangeEvent, event.Topic)
			require.Equal(t, state, event.Data)
		case <-time.After(2 * time.Second):
			t.Fatalf("missing state change to %s", state)
		}
	}
}

type blockedCommandConn struct {
	net.Conn
	started chan struct{}
	release chan struct{}
}

func (c *blockedCommandConn) Write([]byte) (int, error) {
	c.started <- struct{}{}
	<-c.release
	return 0, io.ErrClosedPipe
}

func TestConcurrentFailedStopCommandsRestoreState(t *testing.T) {
	e, _ := attachedEnvironment(t)
	e.emitter = events.NewBus()
	e.meta.Stop = remote.ProcessStopConfiguration{Type: "command", Value: "stop"}
	conn := &blockedCommandConn{Conn: e.stream.Conn, started: make(chan struct{}, 2), release: make(chan struct{})}
	e.SetStream(&types.HijackedResponse{Conn: conn})
	done := make(chan error, 2)
	go func() { done <- e.SendCommand("stop") }()
	<-conn.started
	go func() { done <- e.SendCommand("stop") }()
	close(conn.release)
	for range 2 {
		require.ErrorIs(t, <-done, io.ErrClosedPipe)
	}
	require.Equal(t, environment.ProcessRunningState, e.State())
}

func TestConfiguredMultilineStopCommands(t *testing.T) {
	for _, command := range []string{"stop\n", "say goodbye\nstop", "say goodbye\r\nstop\r\n"} {
		t.Run(command, func(t *testing.T) {
			e, input := attachedEnvironment(t)
			e.emitter = events.NewBus()
			e.meta.Stop = remote.ProcessStopConfiguration{Type: "command", Value: command}
			read := make(chan struct{})
			go func() {
				defer close(read)
				_, _ = io.CopyN(io.Discard, input, int64(len(command)+1))
			}()
			require.NoError(t, e.Stop(context.Background()))
			<-read
			require.Equal(t, environment.ProcessStoppingState, e.State())
		})
	}
}

type partialCommandConn struct {
	net.Conn
	limit int
}

func (c *partialCommandConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b[:c.limit])
	if err != nil {
		return n, err
	}
	return n, io.ErrClosedPipe
}

func TestPartialStopCommandDelivery(t *testing.T) {
	for _, tc := range []struct {
		command, stop string
		written       int
		state         string
	}{
		{"stop\ntrailing", "stop", len("stop\n"), environment.ProcessStoppingState},
		{"stop\ntrailing", "stop", len("stop"), environment.ProcessRunningState},
		{"say goodbye\nstop\ntrailing", "say goodbye\nstop", len("say goodbye\nstop\n"), environment.ProcessStoppingState},
		{"say goodbye\nstop\ntrailing", "say goodbye\nstop", len("say goodbye\n"), environment.ProcessRunningState},
	} {
		t.Run(tc.command+"/"+tc.state, func(t *testing.T) {
			e, input := attachedEnvironment(t)
			e.emitter = events.NewBus()
			e.meta.Stop = remote.ProcessStopConfiguration{Type: "command", Value: tc.stop}
			e.SetStream(&types.HijackedResponse{Conn: &partialCommandConn{Conn: e.stream.Conn, limit: tc.written}})
			read := make(chan struct{})
			go func() { defer close(read); _, _ = io.CopyN(io.Discard, input, int64(tc.written)) }()
			require.ErrorIs(t, e.SendCommand(tc.command), io.ErrClosedPipe)
			<-read
			require.Equal(t, tc.state, e.State())
		})
	}
}

type signaledCommandConn struct {
	net.Conn
	started chan struct{}
}

func (c *signaledCommandConn) Write(b []byte) (int, error) {
	c.started <- struct{}{}
	return c.Conn.Write(b)
}

func TestStopContextCancelsQueuedAndBlockedCommands(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(map[bool]string{false: "writing", true: "queued"}[queued], func(t *testing.T) {
			e, input := attachedEnvironment(t)
			e.emitter = events.NewBus()
			e.meta.Stop = remote.ProcessStopConfiguration{Type: "command", Value: "stop"}
			conn := &signaledCommandConn{Conn: e.stream.Conn, started: make(chan struct{}, 2)}
			e.SetStream(&types.HijackedResponse{Conn: conn})
			if queued {
				first := make(chan error, 1)
				go func() { first <- e.SendCommand("say hello") }()
				<-conn.started
				defer func() { _ = input.Close(); <-first }()
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- e.Stop(ctx) }()
			if !queued {
				<-conn.started
			}
			cancel()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(time.Second):
				_ = input.Close()
				<-done
				t.Fatal("stop ignored cancellation")
			}
			require.Equal(t, environment.ProcessRunningState, e.State())
		})
	}
}
