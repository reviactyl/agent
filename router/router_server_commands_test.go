package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/reviactyl/agent/environment"
	"github.com/reviactyl/agent/server"
)

type commandTestEnvironment struct {
	environment.ProcessEnvironment
	failures  map[string]bool
	attempted []string
	delivered []string
}

func (*commandTestEnvironment) IsRunning(context.Context) (bool, error) { return true, nil }

func (e *commandTestEnvironment) SendCommand(command string) error {
	e.attempted = append(e.attempted, command)
	if e.failures[command] {
		return io.ErrClosedPipe
	}
	e.delivered = append(e.delivered, command)
	return nil
}

func TestPostServerCommands(t *testing.T) {
	for _, tt := range []struct {
		name      string
		commands  []string
		failures  map[string]bool
		delivered []string
		status    int
	}{
		{name: "success", commands: []string{"first", "last"}, delivered: []string{"first", "last"}, status: http.StatusNoContent},
		{name: "empty", commands: []string{}, status: http.StatusNoContent},
		{name: "mixed", commands: []string{"first", "broken", "last"}, failures: map[string]bool{"broken": true}, delivered: []string{"first", "last"}, status: http.StatusInternalServerError},
		{name: "first fails", commands: []string{"broken", "last"}, failures: map[string]bool{"broken": true}, delivered: []string{"last"}, status: http.StatusInternalServerError},
		{name: "last fails", commands: []string{"first", "broken"}, failures: map[string]bool{"broken": true}, delivered: []string{"first"}, status: http.StatusInternalServerError},
		{name: "all fail", commands: []string{"broken", "also broken"}, failures: map[string]bool{"broken": true, "also broken": true}, status: http.StatusInternalServerError},
	} {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			env := &commandTestEnvironment{failures: tt.failures}
			s, err := server.New(backupTestRemoteClient{})
			if err != nil {
				t.Fatal(err)
			}
			defer s.CtxCancel()
			s.Environment = env
			body, err := json.Marshal(map[string][]string{"commands": tt.commands})
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/servers/server/commands", strings.NewReader(string(body)))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set("server", s)

			postServerCommands(c)

			if c.Writer.Status() != tt.status {
				t.Errorf("status = %d, want %d; body = %s", c.Writer.Status(), tt.status, w.Body.String())
			}
			if len(env.attempted) != len(tt.commands) || (len(tt.commands) > 0 && !reflect.DeepEqual(env.attempted, tt.commands)) {
				t.Errorf("attempted = %v, want %v", env.attempted, tt.commands)
			}
			if !reflect.DeepEqual(env.delivered, tt.delivered) {
				t.Errorf("delivered = %v, want %v", env.delivered, tt.delivered)
			}
			if tt.status == http.StatusInternalServerError {
				var response struct {
					Error string `json:"error"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Error == "" {
					t.Fatalf("expected JSON error, got %q (error: %v)", w.Body.String(), err)
				}
			}
		})
	}
}
