package common

import (
	"context"
	"fmt"
	"log"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssmincidents"
)

// AwsIncidentManagerProvider implements the OnCallProvider interface for AWS Incident Manager
type AwsIncidentManagerProvider struct {
	client          *ssmincidents.Client
	responsePlanArn string
}

// NewAwsIncidentManagerProvider creates a new AWS Incident Manager provider
func NewAwsIncidentManagerProvider(client *ssmincidents.Client, responsePlanArn string) *AwsIncidentManagerProvider {
	return &AwsIncidentManagerProvider{
		client:          client,
		responsePlanArn: responsePlanArn,
	}
}

// TriggerOnCall creates an incident in AWS Incident Manager
func (p *AwsIncidentManagerProvider) TriggerOnCall(ctx context.Context, incidentID string, cfg *config.OnCallConfig) error {
	// Use the override config if provided, otherwise use the default
	responsePlanArn := p.responsePlanArn
	if cfg != nil && cfg.AwsIncidentManager.ResponsePlanArn != "" {
		responsePlanArn = cfg.AwsIncidentManager.ResponsePlanArn
	}

	title := "Incident id " + incidentID
	input := &ssmincidents.StartIncidentInput{
		ResponsePlanArn: aws.String(responsePlanArn),
		Title:           aws.String(title),
	}

	if _, err := p.client.StartIncident(ctx, input); err != nil {
		return fmt.Errorf("failed to start AWS incident: %v", err)
	}

	log.Printf("AWS Incident escalated: %s", title)
	return nil
}
