package cli

import (
	"net"
	"testing"

	"github.com/spf13/cobra"
)

func TestResolveTunnelBindAliasesAndAddresses(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    string
		wantErr bool
	}{
		{value: "127.0.0.1", want: "127.0.0.1"},
		{value: "localhost", want: "127.0.0.1"},
		{value: "all", want: "0.0.0.0"},
		{value: "*", want: "0.0.0.0"},
		{value: "192.168.1.20", want: "192.168.1.20"},
		{value: "::1", wantErr: true},
		{value: "definitely-not-an-interface", wantErr: true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			cmd := &cobra.Command{Use: "test"}
			cmd.Flags().String("bind", "127.0.0.1", "")
			if err := cmd.Flags().Set("bind", tc.value); err != nil {
				t.Fatal(err)
			}
			got, err := resolveTunnelBind(cmd)
			if tc.wantErr {
				if err == nil {
					t.Fatal("resolveTunnelBind() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveTunnelBind() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("resolveTunnelBind() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveTunnelBindInterfaceName(t *testing.T) {
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range interfaces {
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err != nil || ip.To4() == nil {
				continue
			}
			cmd := &cobra.Command{Use: "test"}
			cmd.Flags().String("bind", "127.0.0.1", "")
			_ = cmd.Flags().Set("bind", iface.Name)
			got, err := resolveTunnelBind(cmd)
			if err != nil {
				t.Fatalf("resolveTunnelBind(%q) error = %v", iface.Name, err)
			}
			if net.ParseIP(got).To4() == nil {
				t.Fatalf("resolveTunnelBind(%q) = %q, want IPv4 address", iface.Name, got)
			}
			return
		}
	}
	t.Skip("no interface with an IPv4 address")
}

func TestParseTunnelPort(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   string
		want    int
		wantErr bool
	}{
		{name: "valid", value: "5432", want: 5432},
		{name: "trimmed", value: " 3000 ", want: 3000},
		{name: "minimum", value: "1", want: 1},
		{name: "maximum", value: "65535", want: 65535},
		{name: "zero", value: "0", wantErr: true},
		{name: "too large", value: "65536", wantErr: true},
		{name: "negative", value: "-1", wantErr: true},
		{name: "not numeric", value: "postgres", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTunnelPort(tc.value, "container port")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseTunnelPort(%q) error = nil, want error", tc.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTunnelPort(%q) error = %v", tc.value, err)
			}
			if got != tc.want {
				t.Fatalf("parseTunnelPort(%q) = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}

func TestResolveTunnelPorts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		portArg  string
		hostPort string
		wantHost int
		wantAuto bool
		wantErr  bool
	}{
		{name: "default auto", portArg: "5432", hostPort: "auto", wantHost: 5432, wantAuto: true},
		{name: "case insensitive auto", portArg: "5432", hostPort: "AUTO", wantHost: 5432, wantAuto: true},
		{name: "explicit host", portArg: "5432", hostPort: "15432", wantHost: 15432},
		{name: "invalid host", portArg: "5432", hostPort: "65536", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{Use: "test"}
			cmd.Flags().String("host-port", "auto", "")
			if err := cmd.Flags().Set("host-port", tc.hostPort); err != nil {
				t.Fatal(err)
			}
			containerPort, hostPort, auto, err := resolveTunnelPorts(tc.portArg, cmd)
			if tc.wantErr {
				if err == nil {
					t.Fatal("resolveTunnelPorts() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveTunnelPorts() error = %v", err)
			}
			if containerPort != 5432 || hostPort != tc.wantHost || auto != tc.wantAuto {
				t.Fatalf("resolveTunnelPorts() = (%d, %d, %t), want (5432, %d, %t)", containerPort, hostPort, auto, tc.wantHost, tc.wantAuto)
			}
		})
	}
}
