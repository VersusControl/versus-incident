package weborigin

import "testing"

func TestNormalize(t *testing.T) {
	tests := []struct {
		name, raw, want string
		wantErr         bool
	}{
		{name: "https casing and default port", raw: "HTTPS://Console.Example.COM:443/", want: "https://console.example.com"},
		{name: "http default port", raw: "http://Console.Example.COM:80", want: "http://console.example.com"},
		{name: "custom port", raw: "https://console.example.com:8443", want: "https://console.example.com:8443"},
		{name: "relative", raw: "console.example.com", wantErr: true},
		{name: "path", raw: "https://console.example.com/admin", wantErr: true},
		{name: "query", raw: "https://console.example.com?mode=admin", wantErr: true},
		{name: "fragment", raw: "https://console.example.com#admin", wantErr: true},
		{name: "userinfo", raw: "https://user@console.example.com", wantErr: true},
		{name: "scheme", raw: "ftp://console.example.com", wantErr: true},
		{name: "invalid port", raw: "https://console.example.com:70000", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Normalize(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Normalize() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("Normalize() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSame(t *testing.T) {
	tests := []struct {
		name, candidate, expected string
		allowPath, want           bool
	}{
		{name: "default ports", candidate: "https://CONSOLE.example:443", expected: "https://console.example", want: true},
		{name: "referer path", candidate: "https://console.example/admin?tab=one", expected: "https://console.example", allowPath: true, want: true},
		{name: "origin path", candidate: "https://console.example/admin", expected: "https://console.example"},
		{name: "prefix hostname", candidate: "https://console.example.attacker.test/admin", expected: "https://console.example", allowPath: true},
		{name: "scheme mismatch", candidate: "http://console.example", expected: "https://console.example"},
		{name: "port mismatch", candidate: "https://console.example:8443", expected: "https://console.example"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Same(tt.candidate, tt.expected, tt.allowPath); got != tt.want {
				t.Fatalf("Same() = %v, want %v", got, tt.want)
			}
		})
	}
}
