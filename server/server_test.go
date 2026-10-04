package server

import (
	"slices"
	"testing"

	"github.com/reviactyl/agent/config"
	"github.com/reviactyl/agent/environment"
)

func TestEnvironmentVariablesTimezone(t *testing.T) {
	config.Set(&config.Configuration{
		AuthenticationToken: "abc",
		System:              config.SystemConfiguration{Timezone: "Europe/London"},
	})

	tests := []struct {
		name string
		vars environment.Variables
		want string
	}{
		{name: "defaults to the system timezone", vars: environment.Variables{}, want: "TZ=Europe/London"},
		{name: "uses the server timezone", vars: environment.Variables{"TZ": "America/New_York"}, want: "TZ=America/New_York"},
		{name: "ignores an empty server timezone", vars: environment.Variables{"TZ": ""}, want: "TZ=Europe/London"},
		{name: "ignores the local timezone", vars: environment.Variables{"TZ": "Local"}, want: "TZ=Europe/London"},
		{name: "skips an empty key of another case", vars: environment.Variables{"TZ": "America/New_York", "tz": ""}, want: "TZ=America/New_York"},
		{name: "prefers the uppercase key", vars: environment.Variables{"TZ": "America/New_York", "tz": "Asia/Tokyo"}, want: "TZ=America/New_York"},
		{name: "ignores an unknown server timezone", vars: environment.Variables{"TZ": "Not/AZone"}, want: "TZ=Europe/London"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{}
			s.cfg.EnvVars = tt.vars

			var got []string
			for _, v := range s.GetEnvironmentVariables() {
				if len(v) > 3 && v[:3] == "TZ=" {
					got = append(got, v)
				}
			}

			if !slices.Equal(got, []string{tt.want}) {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
		})
	}
}
