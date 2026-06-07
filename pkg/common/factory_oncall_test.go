package common

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
)

func newFactoryForProvider(oncall config.OnCallConfig) *OnCallProviderFactory {
	cfg := &config.Config{
		OnCall: oncall,
	}
	return NewOnCallProviderFactory(cfg, nil)
}

func TestCreateProvider_ServiceNow(t *testing.T) {
	factory := newFactoryForProvider(config.OnCallConfig{
		Provider: "servicenow",
		ServiceNow: config.ServiceNowConfig{
			InstanceURL: "https://dev12345.service-now.com",
			Username:    "admin",
			Password:    "s3cret",
		},
	})

	provider, err := factory.CreateProvider()
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if _, ok := provider.(*ServiceNowProvider); !ok {
		t.Errorf("expected *ServiceNowProvider, got %T", provider)
	}
}

func TestCreateProvider_ServiceNow_MissingInstanceURL(t *testing.T) {
	factory := newFactoryForProvider(config.OnCallConfig{
		Provider: "servicenow",
		ServiceNow: config.ServiceNowConfig{
			Username: "admin",
			Password: "s3cret",
		},
	})

	if _, err := factory.CreateProvider(); err == nil {
		t.Fatalf("expected error for missing instance URL, got nil")
	}
}

func TestCreateProvider_ServiceNow_MissingCredentials(t *testing.T) {
	factory := newFactoryForProvider(config.OnCallConfig{
		Provider: "servicenow",
		ServiceNow: config.ServiceNowConfig{
			InstanceURL: "https://dev12345.service-now.com",
		},
	})

	if _, err := factory.CreateProvider(); err == nil {
		t.Fatalf("expected error for missing credentials, got nil")
	}
}

func TestCreateProvider_Incidentio(t *testing.T) {
	factory := newFactoryForProvider(config.OnCallConfig{
		Provider: "incident_io",
		Incidentio: config.IncidentioConfig{
			APIKey:              "test-key",
			AlertSourceConfigID: "src-123",
		},
	})

	provider, err := factory.CreateProvider()
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if _, ok := provider.(*IncidentioProvider); !ok {
		t.Errorf("expected *IncidentioProvider, got %T", provider)
	}
}

func TestCreateProvider_Incidentio_MissingAPIKey(t *testing.T) {
	factory := newFactoryForProvider(config.OnCallConfig{
		Provider: "incident_io",
		Incidentio: config.IncidentioConfig{
			AlertSourceConfigID: "src-123",
		},
	})

	if _, err := factory.CreateProvider(); err == nil {
		t.Fatalf("expected error for missing API key, got nil")
	}
}

func TestCreateProvider_Incidentio_MissingAlertSource(t *testing.T) {
	factory := newFactoryForProvider(config.OnCallConfig{
		Provider: "incident_io",
		Incidentio: config.IncidentioConfig{
			APIKey: "test-key",
		},
	})

	if _, err := factory.CreateProvider(); err == nil {
		t.Fatalf("expected error for missing alert source config id, got nil")
	}
}

func TestCreateProvider_Unsupported(t *testing.T) {
	factory := newFactoryForProvider(config.OnCallConfig{
		Provider: "does_not_exist",
	})

	if _, err := factory.CreateProvider(); err == nil {
		t.Fatalf("expected error for unsupported provider, got nil")
	}
}
