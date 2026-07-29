package carrier_test

import (
	"testing"

	"github.com/tholent/chaskiwasi/internal/carrier"
	"github.com/tholent/chaskiwasi/internal/config"
)

func TestNew_EmptyNameMeansNoCarrier(t *testing.T) {
	c, err := carrier.New(config.Carrier{}, "some-key")
	if err != nil {
		t.Fatalf("New with empty name: %v", err)
	}
	if c != nil {
		t.Fatalf("New with empty name = %v, want nil (§10.4: no carrier configured)", c)
	}
}

func TestNew_Fake(t *testing.T) {
	c, err := carrier.New(config.Carrier{Name: "fake"}, "")
	if err != nil {
		t.Fatalf("New(fake): %v", err)
	}
	if c == nil || c.Name() != "fake" {
		t.Fatalf("New(fake) = %v, want a fake carrier", c)
	}
}

func TestNew_Hologram(t *testing.T) {
	tests := []struct {
		name    string
		options map[string]any
		apiKey  string
		wantErr bool
	}{
		{
			name:    "device id as int64 (typical TOML integer)",
			options: map[string]any{"device_id": int64(4242)},
			apiKey:  "k",
		},
		{
			name:    "device id as string",
			options: map[string]any{"device_id": "4242"},
			apiKey:  "k",
		},
		{
			name:    "with org id",
			options: map[string]any{"device_id": int64(4242), "org_id": "org-1"},
			apiKey:  "k",
		},
		{
			name:    "org id as integer, coerced to string",
			options: map[string]any{"device_id": int64(4242), "org_id": int64(9)},
			apiKey:  "k",
		},
		{
			name:    "missing device id",
			options: map[string]any{},
			apiKey:  "k",
			wantErr: true,
		},
		{
			name:    "missing api key",
			options: map[string]any{"device_id": int64(4242)},
			apiKey:  "",
			wantErr: true,
		},
		{
			name:    "device id wrong type",
			options: map[string]any{"device_id": true},
			apiKey:  "k",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := carrier.New(config.Carrier{Name: "hologram", Options: tt.options}, tt.apiKey)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("New(hologram, %+v): got no error, want one", tt.options)
				}
				return
			}
			if err != nil {
				t.Fatalf("New(hologram, %+v): %v", tt.options, err)
			}
			if c == nil || c.Name() != "hologram" {
				t.Fatalf("New(hologram) = %v, want a hologram carrier", c)
			}
		})
	}
}

func TestNew_SoracomIsAnHonestStub(t *testing.T) {
	c, err := carrier.New(config.Carrier{Name: "soracom"}, "k")
	if err == nil {
		t.Fatal("New(soracom) returned no error, want a clear not-implemented error")
	}
	if c != nil {
		t.Fatalf("New(soracom) = %v, want nil alongside the error", c)
	}
}

func TestNew_UnknownProvider(t *testing.T) {
	_, err := carrier.New(config.Carrier{Name: "carrierpigeon"}, "k")
	if err == nil {
		t.Fatal("New with an unknown provider name returned no error")
	}
}
