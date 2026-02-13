package localproxy

import (
	"testing"
)

func TestDetectDockerSocketSourceWith(t *testing.T) {
	home := "/home/tester"
	defaultSock := "/var/run/docker.sock"
	homeDockerSock := "/home/tester/.docker/run/docker.sock"
	homeOrbSock := "/home/tester/.orbstack/run/docker.sock"
	customUnixSock := "/tmp/custom/docker.sock"

	tests := []struct {
		name       string
		dockerHost string
		homeDir    string
		existing   map[string]bool
		want       string
	}{
		{
			name:       "uses DOCKER_HOST unix socket when present",
			dockerHost: "unix://" + customUnixSock,
			homeDir:    home,
			existing:   map[string]bool{customUnixSock: true, defaultSock: true},
			want:       customUnixSock,
		},
		{
			name:       "falls back from missing unix DOCKER_HOST to default socket",
			dockerHost: "unix:///tmp/missing.sock",
			homeDir:    home,
			existing:   map[string]bool{defaultSock: true},
			want:       defaultSock,
		},
		{
			name:       "falls back from missing unix DOCKER_HOST to home docker socket",
			dockerHost: "unix:///tmp/missing.sock",
			homeDir:    home,
			existing:   map[string]bool{homeDockerSock: true},
			want:       homeDockerSock,
		},
		{
			name:       "uses home docker socket when DOCKER_HOST unset and default missing",
			dockerHost: "",
			homeDir:    home,
			existing:   map[string]bool{homeDockerSock: true},
			want:       homeDockerSock,
		},
		{
			name:       "uses home orbstack socket when docker sockets missing",
			dockerHost: "",
			homeDir:    home,
			existing:   map[string]bool{homeOrbSock: true},
			want:       homeOrbSock,
		},
		{
			name:       "non-unix docker host disables socket-based auto-heal",
			dockerHost: "tcp://127.0.0.1:2375",
			homeDir:    home,
			existing:   map[string]bool{defaultSock: true, homeDockerSock: true, homeOrbSock: true},
			want:       "",
		},
		{
			name:       "returns empty when no candidate exists",
			dockerHost: "",
			homeDir:    home,
			existing:   map[string]bool{},
			want:       "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := detectDockerSocketSourceWith(tc.dockerHost, tc.homeDir, func(path string) bool {
				return tc.existing[path]
			})
			if got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}
