package config

import (
	"errors"
	"testing"
)

func TestServerConfigURLFor(t *testing.T) {
	srv := &ServerConfig{
		Name:     "bastion",
		Internal: "  http://192.168.1.10:2280/  ",
		External: "http://bastion.example.com:2280/",
		Username: "testuser",
	}
	tests := []struct {
		name   string
		kind   string
		want   string
		wantOK bool
	}{
		{name: "internal is trimmed", kind: KindInternal, want: "http://192.168.1.10:2280/", wantOK: true},
		{name: "external", kind: KindExternal, want: "http://bastion.example.com:2280/", wantOK: true},
		{name: "unknown kind", kind: "vpn", wantOK: false},
		{name: "empty kind", kind: "", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := srv.URLFor(tt.kind)
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("URLFor(%q) = (%q, %v), want (%q, %v)", tt.kind, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestServerConfigURLForEmptyAddressIsAbsent(t *testing.T) {
	// A single-address server must report the missing kind as absent so the
	// failover candidate list never contains an empty URL.
	srv := &ServerConfig{Name: "solo", Internal: "   ", External: "https://ext.example.com"}
	if got, ok := srv.URLFor(KindInternal); ok {
		t.Fatalf("URLFor(internal) = (%q, true), want absent", got)
	}
	if got, ok := srv.URLFor(KindExternal); !ok || got != "https://ext.example.com" {
		t.Fatalf("URLFor(external) = (%q, %v), want the external URL", got, ok)
	}
}

func TestServerConfigURLForNilReceiver(t *testing.T) {
	var srv *ServerConfig
	if got, ok := srv.URLFor(KindInternal); ok || got != "" {
		t.Fatalf("nil URLFor = (%q, %v), want (\"\", false)", got, ok)
	}
}

func TestServerConfigPreferred(t *testing.T) {
	tests := []struct {
		name   string
		prefer string
		want   string
	}{
		{name: "unset means internal", prefer: "", want: KindInternal},
		{name: "internal", prefer: "internal", want: KindInternal},
		{name: "external", prefer: "external", want: KindExternal},
		{name: "case insensitive", prefer: "External", want: KindExternal},
		{name: "unknown falls back to internal", prefer: "both", want: KindInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := &ServerConfig{Name: "x", Prefer: tt.prefer}
			if got := srv.Preferred(); got != tt.want {
				t.Fatalf("Preferred() = %q, want %q", got, tt.want)
			}
		})
	}
	var nilSrv *ServerConfig
	if got := nilSrv.Preferred(); got != KindInternal {
		t.Fatalf("nil Preferred() = %q, want %q", got, KindInternal)
	}
}

func TestServerConfigPort(t *testing.T) {
	tests := []struct {
		name string
		port int
		want int
	}{
		{name: "unset means default", port: 0, want: DefaultSSHPort},
		{name: "explicit", port: 2222, want: 2222},
		{name: "custom", port: 2200, want: 2200},
		{name: "negative is out of range", port: -1, want: DefaultSSHPort},
		{name: "above 65535 is out of range", port: 65536, want: DefaultSSHPort},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := &ServerConfig{Name: "x", SSHPort: tt.port}
			if got := srv.Port(); got != tt.want {
				t.Fatalf("Port() = %d, want %d", got, tt.want)
			}
		})
	}
	var nilSrv *ServerConfig
	if got := nilSrv.Port(); got != DefaultSSHPort {
		t.Fatalf("nil Port() = %d, want %d", got, DefaultSSHPort)
	}
}

func TestValidateMatrix(t *testing.T) {
	valid := func() *ServerConfig {
		return &ServerConfig{
			Name:     "bastion",
			Internal: "http://192.168.1.10:2280/",
			External: "http://bastion.example.com:2280/",
			Username: "testuser",
		}
	}
	with := func(mutate func(*ServerConfig)) *ServerConfig {
		s := valid()
		mutate(s)
		return s
	}

	tests := []struct {
		name    string
		srv     *ServerConfig
		wantErr bool
	}{
		{name: "both addresses", srv: valid()},
		{name: "internal only", srv: with(func(s *ServerConfig) { s.External = "" })},
		{name: "external only", srv: with(func(s *ServerConfig) { s.Internal = "" })},
		{name: "neither address", srv: with(func(s *ServerConfig) { s.Internal, s.External = "", "" }), wantErr: true},
		{name: "whitespace-only addresses", srv: with(func(s *ServerConfig) { s.Internal, s.External = "  ", "\t" }), wantErr: true},
		{name: "empty username", srv: with(func(s *ServerConfig) { s.Username = "" }), wantErr: true},
		{name: "whitespace-only username", srv: with(func(s *ServerConfig) { s.Username = "   " }), wantErr: true},
		{name: "empty name", srv: with(func(s *ServerConfig) { s.Name = "" }), wantErr: true},
		{name: "nil server", srv: nil, wantErr: true},
		{name: "prefer internal", srv: with(func(s *ServerConfig) { s.Prefer = PreferInternal })},
		{name: "prefer external", srv: with(func(s *ServerConfig) { s.Prefer = PreferExternal })},
		{name: "prefer empty is allowed", srv: with(func(s *ServerConfig) { s.Prefer = "" })},
		{name: "bad prefer", srv: with(func(s *ServerConfig) { s.Prefer = "both" }), wantErr: true},
		{name: "ssh_port 0 means default", srv: with(func(s *ServerConfig) { s.SSHPort = 0 })},
		{name: "ssh_port 2222", srv: with(func(s *ServerConfig) { s.SSHPort = 2222 })},
		{name: "ssh_port 1", srv: with(func(s *ServerConfig) { s.SSHPort = 1 })},
		{name: "ssh_port 65535", srv: with(func(s *ServerConfig) { s.SSHPort = 65535 })},
		{name: "ssh_port 65536", srv: with(func(s *ServerConfig) { s.SSHPort = 65536 }), wantErr: true},
		{name: "ssh_port negative", srv: with(func(s *ServerConfig) { s.SSHPort = -1 }), wantErr: true},
		{name: "https scheme", srv: with(func(s *ServerConfig) { s.Internal = "https://jump.example.com:2280/" })},
		{name: "non-http scheme", srv: with(func(s *ServerConfig) { s.Internal = "ftp://jump.example.com/" }), wantErr: true},
		{name: "bare host and port has no scheme", srv: with(func(s *ServerConfig) { s.Internal = "192.168.1.10:2280" }), wantErr: true},
		{name: "scheme-relative URL", srv: with(func(s *ServerConfig) { s.Internal = "//jump.example.com/" }), wantErr: true},
		{name: "missing host", srv: with(func(s *ServerConfig) { s.Internal = "http://" }), wantErr: true},
		{name: "missing host with path", srv: with(func(s *ServerConfig) { s.Internal = "http:///api" }), wantErr: true},
		{name: "bad external while internal is fine", srv: with(func(s *ServerConfig) { s.External = "ssh://jump.example.com" }), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.srv)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Validate() = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestAppConfigPutNormalizesAndDefaults(t *testing.T) {
	cfg := &AppConfig{}
	err := cfg.Put(&ServerConfig{
		Name:     "  bastion  ",
		Internal: "  http://192.168.1.10:2280/  ",
		Username: "  testuser  ",
		Prefer:   "  EXTERNAL  ",
	})
	if err != nil {
		t.Fatalf("Put() = %v, want nil", err)
	}
	srv, err := cfg.Get("bastion")
	if err != nil {
		t.Fatalf("Get() = %v, want the trimmed alias", err)
	}
	if srv.Internal != "http://192.168.1.10:2280/" {
		t.Errorf("Internal = %q, want trimmed", srv.Internal)
	}
	if srv.Username != "testuser" {
		t.Errorf("Username = %q, want trimmed", srv.Username)
	}
	if srv.Prefer != PreferExternal {
		t.Errorf("Prefer = %q, want %q", srv.Prefer, PreferExternal)
	}
	if cfg.Version != ConfigVersion {
		t.Errorf("Version = %v, want %v", cfg.Version, ConfigVersion)
	}
	if cfg.Default != "bastion" {
		t.Errorf("Default = %q, want the first server to become the default", cfg.Default)
	}

	// A second server must not steal the default.
	if err := cfg.Put(&ServerConfig{Name: "alpha", Internal: "https://a.example.com", Username: "u"}); err != nil {
		t.Fatalf("Put() = %v, want nil", err)
	}
	if cfg.Default != "bastion" {
		t.Errorf("Default = %q, want the first server to stay the default", cfg.Default)
	}
}

func TestAppConfigPutRejectsInvalid(t *testing.T) {
	cfg := &AppConfig{}
	err := cfg.Put(&ServerConfig{Name: "broken", Username: "u"})
	if err == nil {
		t.Fatal("Put() = nil, want a validation error")
	}
	if _, getErr := cfg.Get("broken"); getErr == nil {
		t.Fatal("Put() stored an invalid server")
	}
}

func TestAppConfigPutNilReceiverDoesNotPanic(t *testing.T) {
	var cfg *AppConfig
	if err := cfg.Put(&ServerConfig{Name: "x", Internal: "https://a/", Username: "u"}); err == nil {
		t.Fatal("Put() on a nil receiver = nil, want an error")
	}
	if err := cfg.Put(nil); err == nil {
		t.Fatal("Put(nil) = nil, want an error")
	}
}

func TestAppConfigNames(t *testing.T) {
	cfg := &AppConfig{Servers: map[string]*ServerConfig{
		"zeta":  {Name: "zeta"},
		"alpha": {Name: "alpha"},
		"mid":   {Name: "mid"},
	}}
	got := cfg.Names()
	want := []string{"alpha", "mid", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() = %v, want %v", got, want)
		}
	}
	var nilCfg *AppConfig
	if names := nilCfg.Names(); len(names) != 0 {
		t.Fatalf("nil Names() = %v, want empty", names)
	}
}

func TestAppConfigGet(t *testing.T) {
	cfg := &AppConfig{Servers: map[string]*ServerConfig{
		"alpha": {Name: "alpha", Internal: "https://a/", Username: "u"},
		"beta":  {Name: "beta", Internal: "https://b/", Username: "u"},
	}}
	srv, err := cfg.Get("beta")
	if err != nil || srv.Name != "beta" {
		t.Fatalf("Get(beta) = (%v, %v), want beta", srv, err)
	}
	_, err = cfg.Get("missing")
	if !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("Get(missing) = %v, want ErrServerNotFound", err)
	}
	// The message must name the available aliases so the user can recover.
	if msg := err.Error(); msg == "" {
		t.Fatal("Get(missing) produced an empty message")
	}
	var nilCfg *AppConfig
	if _, err := nilCfg.Get("x"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("nil Get() = %v, want ErrServerNotFound", err)
	}
}

func TestAppConfigDefaultServer(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *AppConfig
		want    string
		wantErr error
	}{
		{
			name: "explicit default wins",
			cfg: &AppConfig{Default: "beta", Servers: map[string]*ServerConfig{
				"alpha": {Name: "alpha"}, "beta": {Name: "beta"},
			}},
			want: "beta",
		},
		{
			name: "empty default falls back to first alias",
			cfg: &AppConfig{Servers: map[string]*ServerConfig{
				"zeta": {Name: "zeta"}, "alpha": {Name: "alpha"},
			}},
			want: "alpha",
		},
		{
			name: "dangling default falls back to first alias",
			cfg: &AppConfig{Default: "ghost", Servers: map[string]*ServerConfig{
				"zeta": {Name: "zeta"}, "alpha": {Name: "alpha"},
			}},
			want: "alpha",
		},
		{
			name:    "no servers",
			cfg:     &AppConfig{},
			wantErr: ErrNoServers,
		},
		{
			name:    "nil receiver",
			cfg:     nil,
			wantErr: ErrNoServers,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, err := tt.cfg.DefaultServer()
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("DefaultServer() = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("DefaultServer() = %v, want nil", err)
			}
			if srv.Name != tt.want {
				t.Fatalf("DefaultServer() = %q, want %q", srv.Name, tt.want)
			}
		})
	}
}

func TestAppConfigDelete(t *testing.T) {
	t.Run("promotes first remaining when the default is removed", func(t *testing.T) {
		cfg := &AppConfig{Default: "beta", Servers: map[string]*ServerConfig{
			"alpha": {Name: "alpha"}, "beta": {Name: "beta"}, "gamma": {Name: "gamma"},
		}}
		if err := cfg.Delete("beta"); err != nil {
			t.Fatalf("Delete() = %v, want nil", err)
		}
		if cfg.Default != "alpha" {
			t.Fatalf("Default = %q, want the alphabetically first remaining alias", cfg.Default)
		}
	})
	t.Run("clears the default when nothing remains", func(t *testing.T) {
		cfg := &AppConfig{Default: "only", Servers: map[string]*ServerConfig{
			"only": {Name: "only"},
		}}
		if err := cfg.Delete("only"); err != nil {
			t.Fatalf("Delete() = %v, want nil", err)
		}
		if cfg.Default != "" {
			t.Fatalf("Default = %q, want empty", cfg.Default)
		}
	})
	t.Run("keeps the default when a different alias is removed", func(t *testing.T) {
		cfg := &AppConfig{Default: "beta", Servers: map[string]*ServerConfig{
			"alpha": {Name: "alpha"}, "beta": {Name: "beta"},
		}}
		if err := cfg.Delete("alpha"); err != nil {
			t.Fatalf("Delete() = %v, want nil", err)
		}
		if cfg.Default != "beta" {
			t.Fatalf("Default = %q, want beta", cfg.Default)
		}
	})
	t.Run("unknown alias", func(t *testing.T) {
		cfg := &AppConfig{Servers: map[string]*ServerConfig{"alpha": {Name: "alpha"}}}
		if err := cfg.Delete("ghost"); !errors.Is(err, ErrServerNotFound) {
			t.Fatalf("Delete(ghost) = %v, want ErrServerNotFound", err)
		}
	})
	t.Run("nil receiver", func(t *testing.T) {
		var cfg *AppConfig
		if err := cfg.Delete("x"); !errors.Is(err, ErrServerNotFound) {
			t.Fatalf("nil Delete() = %v, want ErrServerNotFound", err)
		}
	})
}

func TestAppConfigSetDefault(t *testing.T) {
	cfg := &AppConfig{Servers: map[string]*ServerConfig{
		"alpha": {Name: "alpha"}, "beta": {Name: "beta"},
	}}
	if err := cfg.SetDefault("beta"); err != nil {
		t.Fatalf("SetDefault(beta) = %v, want nil", err)
	}
	if cfg.Default != "beta" {
		t.Fatalf("Default = %q, want beta", cfg.Default)
	}
	if err := cfg.SetDefault("ghost"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("SetDefault(ghost) = %v, want ErrServerNotFound", err)
	}
	if cfg.Default != "beta" {
		t.Fatalf("Default = %q, want beta to be unchanged after a failed SetDefault", cfg.Default)
	}
}
