package common

import (
	"fmt"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/aws/aws-sdk-go-v2/service/ssmincidents"
)

// OnCall Provider
type OnCallProviderFactory struct {
	cfg       *config.Config
	awsClient *ssmincidents.Client
}

func NewOnCallProviderFactory(cfg *config.Config, awsClient *ssmincidents.Client) *OnCallProviderFactory {
	return &OnCallProviderFactory{
		cfg:       cfg,
		awsClient: awsClient,
	}
}

func (f *OnCallProviderFactory) CreateProvider() (core.OnCallProvider, error) {
	if f.cfg.OnCall.Provider == "aws_incident_manager" || f.cfg.OnCall.Provider == "" {
		// Default to AWS Incident Manager for backward compatibility
		if f.cfg.OnCall.AwsIncidentManager.ResponsePlanArn == "" {
			return nil, fmt.Errorf("missing Response Plan ARN configuration for AWS Incident Manager")
		}

		return NewAwsIncidentManagerProvider(
			f.awsClient,
			f.cfg.OnCall.AwsIncidentManager.ResponsePlanArn,
		), nil
	} else if f.cfg.OnCall.Provider == "pagerduty" {
		if f.cfg.OnCall.PagerDuty.RoutingKey == "" {
			return nil, fmt.Errorf("missing Routing Key configuration for PagerDuty")
		}

		return NewPagerDutyProvider(f.cfg.OnCall.PagerDuty.RoutingKey), nil
	} else if f.cfg.OnCall.Provider == "servicenow" {
		if f.cfg.OnCall.ServiceNow.InstanceURL == "" {
			return nil, fmt.Errorf("missing Instance URL configuration for ServiceNow")
		}
		if f.cfg.OnCall.ServiceNow.Username == "" || f.cfg.OnCall.ServiceNow.Password == "" {
			return nil, fmt.Errorf("missing Username/Password configuration for ServiceNow")
		}

		return NewServiceNowProvider(
			f.cfg.OnCall.ServiceNow.InstanceURL,
			f.cfg.OnCall.ServiceNow.Username,
			f.cfg.OnCall.ServiceNow.Password,
			f.cfg.OnCall.ServiceNow.Table,
		), nil
	} else if f.cfg.OnCall.Provider == "incident_io" {
		if f.cfg.OnCall.Incidentio.APIKey == "" {
			return nil, fmt.Errorf("missing API Key configuration for incident.io")
		}
		if f.cfg.OnCall.Incidentio.AlertSourceConfigID == "" {
			return nil, fmt.Errorf("missing Alert Source Config ID configuration for incident.io")
		}

		return NewIncidentioProvider(
			f.cfg.OnCall.Incidentio.APIKey,
			f.cfg.OnCall.Incidentio.AlertSourceConfigID,
		), nil
	}

	return nil, fmt.Errorf("unsupported on-call provider: %s", f.cfg.OnCall.Provider)
}

// Initialize the provider factory in core
func init() {
	core.CreateOnCallProvider = CreateOnCallProvider
}

// CreateOnCallProvider is a helper function that creates an on-call provider
// This is used by the core package to create providers without directly importing
// the implementation details
func CreateOnCallProvider(cfg *config.Config, awsClient *ssmincidents.Client) (core.OnCallProvider, error) {
	factory := NewOnCallProviderFactory(cfg, awsClient)
	return factory.CreateProvider()
}
