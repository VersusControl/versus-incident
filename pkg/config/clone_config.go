package config

func cloneConfig(src *Config) *Config {
	if src == nil {
		return nil
	}

	// Create a new Config and copy all fields
	cloned := &Config{
		Name:          src.Name,
		Host:          src.Host,
		Port:          src.Port,
		PublicHost:    src.PublicHost,
		GatewaySecret: src.GatewaySecret,
		Alert:         cloneAlertConfig(src.Alert),
		Queue:         cloneQueueConfig(src.Queue),
		OnCall:        cloneOnCallConfig(src.OnCall),
		Proxy:         cloneProxyConfig(src.Proxy),
		Redis:         cloneRedisConfig(src.Redis),
		Storage:       cloneStorageConfig(src.Storage),
		Agent:         cloneAgentConfig(src.Agent),
	}

	return cloned
}

// Helper function to deep clone the StorageConfig struct
func cloneStorageConfig(src StorageConfig) StorageConfig {
	return StorageConfig{
		Type: src.Type,
		File: StorageFileConfig{
			MaxIncidents: src.File.MaxIncidents,
		},
		Redis: StorageRedisConfig{
			Host:               src.Redis.Host,
			Port:               src.Redis.Port,
			Password:           src.Redis.Password,
			DB:                 src.Redis.DB,
			InsecureSkipVerify: src.Redis.InsecureSkipVerify,
			KeyPrefix:          src.Redis.KeyPrefix,
			MaxIncidents:       src.Redis.MaxIncidents,
		},
		Database: StorageDatabaseConfig{
			Driver:       src.Database.Driver,
			DSN:          src.Database.DSN,
			MaxIncidents: src.Database.MaxIncidents,
		},
	}
}

// Helper function to deep clone the AlertConfig struct
func cloneAlertConfig(src AlertConfig) AlertConfig {
	return AlertConfig{
		DebugBody: src.DebugBody,
		Slack:     cloneSlackConfig(src.Slack),
		Telegram:  cloneTelegramConfig(src.Telegram),
		Viber:     cloneViberConfig(src.Viber),
		Email:     cloneEmailConfig(src.Email),
		MSTeams:   cloneMSTeamsConfig(src.MSTeams),
		Lark:      cloneLarkConfig(src.Lark),
	}
}

// Helper function to deep clone the SlackConfig struct
func cloneSlackConfig(src SlackConfig) SlackConfig {
	return SlackConfig{
		Enable:       src.Enable,
		Token:        src.Token,
		ChannelID:    src.ChannelID,
		TemplatePath: src.TemplatePath,
		MessageProperties: SlackMessageProperties{
			DisableButton: src.MessageProperties.DisableButton,
			ButtonText:    src.MessageProperties.ButtonText,
			ButtonStyle:   src.MessageProperties.ButtonStyle,
		},
	}
}

// Helper function to deep clone the TelegramConfig struct
func cloneTelegramConfig(src TelegramConfig) TelegramConfig {
	return TelegramConfig{
		Enable:       src.Enable,
		BotToken:     src.BotToken,
		ChatID:       src.ChatID,
		TemplatePath: src.TemplatePath,
		UseProxy:     src.UseProxy,
	}
}

// Helper function to deep clone the ViberConfig struct
func cloneViberConfig(src ViberConfig) ViberConfig {
	return ViberConfig{
		Enable:       src.Enable,
		APIType:      src.APIType,
		BotToken:     src.BotToken,
		UserID:       src.UserID,
		TemplatePath: src.TemplatePath,
		ChannelID:    src.ChannelID,
		UseProxy:     src.UseProxy,
	}
}

// Helper function to deep clone the EmailConfig struct
func cloneEmailConfig(src EmailConfig) EmailConfig {
	return EmailConfig{
		Enable:       src.Enable,
		SMTPHost:     src.SMTPHost,
		SMTPPort:     src.SMTPPort,
		Username:     src.Username,
		Password:     src.Password,
		To:           src.To,
		Subject:      src.Subject,
		TemplatePath: src.TemplatePath,
	}
}

// Helper function to deep clone the MSTeamsConfig struct
func cloneMSTeamsConfig(src MSTeamsConfig) MSTeamsConfig {
	// Create a copy of OtherPowerURLs map if it exists
	var otherPowerURLsCopy map[string]string
	if src.OtherPowerURLs != nil {
		otherPowerURLsCopy = make(map[string]string)
		for k, v := range src.OtherPowerURLs {
			otherPowerURLsCopy[k] = v
		}
	}

	return MSTeamsConfig{
		Enable:           src.Enable,
		TemplatePath:     src.TemplatePath,
		PowerAutomateURL: src.PowerAutomateURL,
		OtherPowerURLs:   otherPowerURLsCopy,
	}
}

// Helper function to deep clone the LarkConfig struct
func cloneLarkConfig(src LarkConfig) LarkConfig {
	// Create a copy of OtherWebhookURLs map if it exists
	var otherWebhookURLsCopy map[string]string
	if src.OtherWebhookURLs != nil {
		otherWebhookURLsCopy = make(map[string]string)
		for k, v := range src.OtherWebhookURLs {
			otherWebhookURLsCopy[k] = v
		}
	}

	return LarkConfig{
		Enable:           src.Enable,
		WebhookURL:       src.WebhookURL,
		TemplatePath:     src.TemplatePath,
		OtherWebhookURLs: otherWebhookURLsCopy,
		UseProxy:         src.UseProxy,
	}
}

// Helper function to deep clone the QueueConfig struct
func cloneQueueConfig(src QueueConfig) QueueConfig {
	return QueueConfig{
		Enable: src.Enable,
		SNS:    cloneSNSConfig(src.SNS),
		SQS:    cloneSQSConfig(src.SQS),
		PubSub: clonePubSubConfig(src.PubSub),
		AzBus:  cloneAzBusConfig(src.AzBus),
	}
}

// Helper function to deep clone the SNSConfig struct
func cloneSNSConfig(src SNSConfig) SNSConfig {
	return SNSConfig{
		Enable: src.Enable,
	}
}

// Helper function to deep clone the SQSConfig struct
func cloneSQSConfig(src SQSConfig) SQSConfig {
	return SQSConfig{
		Enable:   src.Enable,
		QueueURL: src.QueueURL,
	}
}

// Helper function to deep clone the PubSubConfig struct
func clonePubSubConfig(src PubSubConfig) PubSubConfig {
	return PubSubConfig{
		Enable: src.Enable,
	}
}

// Helper function to deep clone the AzBusConfig struct
func cloneAzBusConfig(src AzBusConfig) AzBusConfig {
	return AzBusConfig{
		Enable: src.Enable,
	}
}

// Helper function to deep clone the OnCallConfig struct
func cloneOnCallConfig(src OnCallConfig) OnCallConfig {
	return OnCallConfig{
		Enable:             src.Enable,
		InitializedOnly:    src.InitializedOnly,
		WaitMinutes:        src.WaitMinutes,
		Provider:           src.Provider,
		AwsIncidentManager: cloneAwsIncidentManagerConfig(src.AwsIncidentManager),
		PagerDuty:          clonePagerDutyConfig(src.PagerDuty),
		ServiceNow:         cloneServiceNowConfig(src.ServiceNow),
		Incidentio:         cloneIncidentioConfig(src.Incidentio),
	}
}

// Helper function to deep clone the AwsIncidentManagerConfig struct
func cloneAwsIncidentManagerConfig(src AwsIncidentManagerConfig) AwsIncidentManagerConfig {
	// Create a copy of OtherResponsePlanArns map if it exists
	var otherResponsePlanArnsCopy map[string]string
	if src.OtherResponsePlanArns != nil {
		otherResponsePlanArnsCopy = make(map[string]string)
		for k, v := range src.OtherResponsePlanArns {
			otherResponsePlanArnsCopy[k] = v
		}
	}

	return AwsIncidentManagerConfig{
		ResponsePlanArn:       src.ResponsePlanArn,
		OtherResponsePlanArns: otherResponsePlanArnsCopy,
	}
}

// Helper function to deep clone the PagerDutyConfig struct
func clonePagerDutyConfig(src PagerDutyConfig) PagerDutyConfig {
	// Create a copy of OtherRoutingKeys map if it exists
	var otherRoutingKeysCopy map[string]string
	if src.OtherRoutingKeys != nil {
		otherRoutingKeysCopy = make(map[string]string)
		for k, v := range src.OtherRoutingKeys {
			otherRoutingKeysCopy[k] = v
		}
	}

	return PagerDutyConfig{
		RoutingKey:       src.RoutingKey,
		OtherRoutingKeys: otherRoutingKeysCopy,
	}
}

// Helper function to deep clone the ServiceNowConfig struct
func cloneServiceNowConfig(src ServiceNowConfig) ServiceNowConfig {
	// Create a copy of OtherInstanceURLs map if it exists
	var otherInstanceURLsCopy map[string]string
	if src.OtherInstanceURLs != nil {
		otherInstanceURLsCopy = make(map[string]string)
		for k, v := range src.OtherInstanceURLs {
			otherInstanceURLsCopy[k] = v
		}
	}

	return ServiceNowConfig{
		InstanceURL:       src.InstanceURL,
		Username:          src.Username,
		Password:          src.Password,
		Table:             src.Table,
		OtherInstanceURLs: otherInstanceURLsCopy,
	}
}

// Helper function to deep clone the IncidentioConfig struct
func cloneIncidentioConfig(src IncidentioConfig) IncidentioConfig {
	// Create a copy of OtherAlertSourceConfigIDs map if it exists
	var otherAlertSourceConfigIDsCopy map[string]string
	if src.OtherAlertSourceConfigIDs != nil {
		otherAlertSourceConfigIDsCopy = make(map[string]string)
		for k, v := range src.OtherAlertSourceConfigIDs {
			otherAlertSourceConfigIDsCopy[k] = v
		}
	}

	return IncidentioConfig{
		APIKey:                    src.APIKey,
		AlertSourceConfigID:       src.AlertSourceConfigID,
		OtherAlertSourceConfigIDs: otherAlertSourceConfigIDsCopy,
	}
}

// Helper function to deep clone the ProxyConfig struct
func cloneProxyConfig(src ProxyConfig) ProxyConfig {
	return ProxyConfig{
		URL:      src.URL,
		Username: src.Username,
		Password: src.Password,
	}
}

// Helper function to deep clone the RedisConfig struct
func cloneRedisConfig(src RedisConfig) RedisConfig {
	return RedisConfig{
		Host:               src.Host,
		Port:               src.Port,
		Password:           src.Password,
		DB:                 src.DB,
		TLS:                src.TLS,
		InsecureSkipVerify: src.InsecureSkipVerify,
	}
}

// Helper function to deep clone the AgentConfig struct
func cloneAgentConfig(src AgentConfig) AgentConfig {
	cloned := AgentConfig{
		Enable:          src.Enable,
		Mode:            src.Mode,
		PollInterval:    src.PollInterval,
		Lookback:        src.Lookback,
		BatchMax:        src.BatchMax,
		SignalMaxBytes:  src.SignalMaxBytes,
		NewServiceGrace: src.NewServiceGrace,
		ServicePatterns: append([]string(nil), src.ServicePatterns...),
		Redaction: AgentRedactionConfig{
			Enable:    src.Redaction.Enable,
			RedactIPs: src.Redaction.RedactIPs,
		},
		Catalog: AgentCatalogConfig{
			PersistInterval:       src.Catalog.PersistInterval,
			AutoPromoteAfter:      src.Catalog.AutoPromoteAfter,
			SpikeMultiplier:       src.Catalog.SpikeMultiplier,
			SpikeMinFrequency:     src.Catalog.SpikeMinFrequency,
			SpikeMinBaselineCount: src.Catalog.SpikeMinBaselineCount,
		},
		Miner: AgentMinerConfig{
			SimilarityThreshold: src.Miner.SimilarityThreshold,
			TreeDepth:           src.Miner.TreeDepth,
			MaxChildren:         src.Miner.MaxChildren,
		},
		Regex: AgentRegexConfig{
			DefaultPattern: src.Regex.DefaultPattern,
			Metrics:        src.Regex.Metrics,
			Traces:         src.Regex.Traces,
		},
		AI: AgentAIConfig{
			Enable:          src.AI.Enable,
			APIKey:          src.AI.APIKey,
			Provider:        src.AI.Provider,
			Model:           src.AI.Model,
			Temperature:     src.AI.Temperature,
			MaxTokens:       src.AI.MaxTokens,
			MaxCallsPerHour: src.AI.MaxCallsPerHour,
			CacheTTL:        src.AI.CacheTTL,
			Detect:          cloneAgentAITaskConfig(src.AI.Detect),
			Analyze:         cloneAgentAIAnalyzeConfig(src.AI.Analyze),
		},
	}

	if src.Redaction.ExtraPatterns != nil {
		cloned.Redaction.ExtraPatterns = append([]string(nil), src.Redaction.ExtraPatterns...)
	}
	if src.Regex.Rules != nil {
		cloned.Regex.Rules = append([]AgentRegexRule(nil), src.Regex.Rules...)
	}
	if src.Sources != nil {
		cloned.Sources = make([]AgentSourceConfig, len(src.Sources))
		for i, s := range src.Sources {
			c := AgentSourceConfig{
				Name:   s.Name,
				Type:   s.Type,
				Enable: s.Enable,
				Elasticsearch: AgentElasticsearchSourceConfig{
					Username:           s.Elasticsearch.Username,
					Password:           s.Elasticsearch.Password,
					APIKey:             s.Elasticsearch.APIKey,
					InsecureSkipVerify: s.Elasticsearch.InsecureSkipVerify,
					Index:              s.Elasticsearch.Index,
					TimeField:          s.Elasticsearch.TimeField,
					Query:              s.Elasticsearch.Query,
					MessageField:       s.Elasticsearch.MessageField,
					SeverityField:      s.Elasticsearch.SeverityField,
					PageSize:           s.Elasticsearch.PageSize,
				},
				File: AgentFileSourceConfig{
					Path:            s.File.Path,
					Format:          s.File.Format,
					CursorPath:      s.File.CursorPath,
					FromBeginning:   s.File.FromBeginning,
					MaxLineBytes:    s.File.MaxLineBytes,
					MaxLinesPerPull: s.File.MaxLinesPerPull,
					TimestampLayout: s.File.TimestampLayout,
					MessageField:    s.File.MessageField,
					TimestampField:  s.File.TimestampField,
					SeverityField:   s.File.SeverityField,
				},
				Loki: AgentLokiSourceConfig{
					Address:            s.Loki.Address,
					TenantID:           s.Loki.TenantID,
					Username:           s.Loki.Username,
					Password:           s.Loki.Password,
					BearerToken:        s.Loki.BearerToken,
					InsecureSkipVerify: s.Loki.InsecureSkipVerify,
					Query:              s.Loki.Query,
					SeverityField:      s.Loki.SeverityField,
					PageSize:           s.Loki.PageSize,
				},
				CloudWatchLogs: AgentCloudWatchLogsSourceConfig{
					Region:          s.CloudWatchLogs.Region,
					LogGroupName:    s.CloudWatchLogs.LogGroupName,
					LogStreamPrefix: s.CloudWatchLogs.LogStreamPrefix,
					FilterPattern:   s.CloudWatchLogs.FilterPattern,
					PageSize:        s.CloudWatchLogs.PageSize,
				},
				Graylog: AgentGraylogSourceConfig{
					Address:            s.Graylog.Address,
					Username:           s.Graylog.Username,
					Password:           s.Graylog.Password,
					APIToken:           s.Graylog.APIToken,
					InsecureSkipVerify: s.Graylog.InsecureSkipVerify,
					Query:              s.Graylog.Query,
					StreamID:           s.Graylog.StreamID,
					MessageField:       s.Graylog.MessageField,
					SeverityField:      s.Graylog.SeverityField,
					PageSize:           s.Graylog.PageSize,
				},
				Splunk: AgentSplunkSourceConfig{
					Address:            s.Splunk.Address,
					Token:              s.Splunk.Token,
					Username:           s.Splunk.Username,
					Password:           s.Splunk.Password,
					InsecureSkipVerify: s.Splunk.InsecureSkipVerify,
					Search:             s.Splunk.Search,
					TimeField:          s.Splunk.TimeField,
					MessageField:       s.Splunk.MessageField,
					SeverityField:      s.Splunk.SeverityField,
					PageSize:           s.Splunk.PageSize,
				},
			}
			if s.Elasticsearch.Addresses != nil {
				c.Elasticsearch.Addresses = append([]string(nil), s.Elasticsearch.Addresses...)
			}
			if s.Elasticsearch.ExtraFields != nil {
				c.Elasticsearch.ExtraFields = append([]string(nil), s.Elasticsearch.ExtraFields...)
			}
			if s.Loki.ExtraLabels != nil {
				c.Loki.ExtraLabels = append([]string(nil), s.Loki.ExtraLabels...)
			}
			if s.Graylog.Fields != nil {
				c.Graylog.Fields = append([]string(nil), s.Graylog.Fields...)
			}
			if s.Graylog.ExtraFields != nil {
				c.Graylog.ExtraFields = append([]string(nil), s.Graylog.ExtraFields...)
			}
			if s.Splunk.ExtraFields != nil {
				c.Splunk.ExtraFields = append([]string(nil), s.Splunk.ExtraFields...)
			}
			if s.Options != nil {
				c.Options = cloneAnyMap(s.Options)
			}
			cloned.Sources[i] = c
		}
	}
	cloned.Tools = cloneToolsConfig(src.Tools)
	return cloned
}

// cloneToolsConfig deep-copies the per-tool config block (tools.yaml),
// including the slice/string fields each tool owns.
func cloneToolsConfig(src ToolsConfig) ToolsConfig {
	out := ToolsConfig{
		ToolTimeout:   src.ToolTimeout,
		ParallelTools: src.ParallelTools,
	}
	out.RecentChanges.Git.Auth = src.RecentChanges.Git.Auth
	if src.RecentChanges.Git.Repos != nil {
		out.RecentChanges.Git.Repos = make([]RecentChangesGitRepo, len(src.RecentChanges.Git.Repos))
		copy(out.RecentChanges.Git.Repos, src.RecentChanges.Git.Repos)
	}
	if src.DescribeDependencies.Services != nil {
		out.DescribeDependencies.Services = make([]ServiceDependency, len(src.DescribeDependencies.Services))
		for i, s := range src.DescribeDependencies.Services {
			out.DescribeDependencies.Services[i] = ServiceDependency{
				Name:      s.Name,
				DependsOn: append([]string(nil), s.DependsOn...),
			}
		}
	}
	out.FindRunbook = src.FindRunbook
	out.QueryMetrics = src.QueryMetrics
	out.QueryTraces = src.QueryTraces
	return out
}

// cloneAgentAITaskConfig is a value-copy helper (all fields are
// primitives) kept as a named function for symmetry with the other
// clone helpers and so future map/slice fields have an obvious home.
func cloneAgentAITaskConfig(src AgentAITaskConfig) AgentAITaskConfig {
	return AgentAITaskConfig{
		Model:           src.Model,
		Provider:        src.Provider,
		Temperature:     src.Temperature,
		MaxTokens:       src.MaxTokens,
		MaxCallsPerHour: src.MaxCallsPerHour,
		CacheTTL:        src.CacheTTL,
	}
}

// cloneAgentAIAnalyzeConfig copies the analyze override block (model
// override only; the tool-loop knobs live on the shared tools block).
func cloneAgentAIAnalyzeConfig(src AgentAIAnalyzeConfig) AgentAIAnalyzeConfig {
	return AgentAIAnalyzeConfig{
		Model:    src.Model,
		Provider: src.Provider,
	}
}

// cloneAnyMap deep-copies a generic decoded-YAML map (the
// AgentSourceConfig.Options block consumed by registered source types).
// It recurses into nested maps and slices so a per-request cloneConfig
// never shares mutable structure with the global config.
func cloneAnyMap(src map[string]interface{}) map[string]interface{} {
	if src == nil {
		return nil
	}
	out := make(map[string]interface{}, len(src))
	for k, v := range src {
		out[k] = cloneAnyValue(v)
	}
	return out
}

// cloneAnyValue deep-copies one decoded-YAML value (map, slice, or scalar).
func cloneAnyValue(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		return cloneAnyMap(t)
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, e := range t {
			out[i] = cloneAnyValue(e)
		}
		return out
	default:
		// Scalars (string/bool/int/float/nil) are value types — safe to copy.
		return t
	}
}
