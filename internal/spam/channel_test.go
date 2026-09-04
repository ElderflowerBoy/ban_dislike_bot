package spam

import "testing"

func TestDetectChannel(t *testing.T) {
	tests := []struct {
		name     string
		title    string
		username string
		spam     bool
	}{
		{name: "latin vpn", title: "Best VPN | Access", spam: true},
		{name: "cyrillic vpn", title: "Надёжный В.П.Н", spam: true},
		{name: "latin proxy in username", title: "Новости", username: "cheap_proxy_shop", spam: true},
		{name: "cyrillic proxy", title: "П Р О К С И для всех", spam: true},
		{name: "mixed alphabet", title: "Fast VРN", spam: true},
		{name: "mixed proxy", title: "PROXУ service", spam: true},
		{name: "benign", title: "Новости Екатеринбурга", username: "ekb_news", spam: false},
		{name: "near match", title: "Proximity lab", username: "proximity_lab", spam: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := DetectChannel(tt.title, tt.username)
			if result.Spam != tt.spam {
				t.Fatalf("DetectChannel(%q, %q) = %+v, want spam=%t", tt.title, tt.username, result, tt.spam)
			}
		})
	}
}
