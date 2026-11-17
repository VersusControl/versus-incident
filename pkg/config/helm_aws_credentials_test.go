package config

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// renderedEnvVar is one container env entry of a rendered Deployment.
type renderedEnvVar struct {
	Name      string  `yaml:"name"`
	Value     *string `yaml:"value"`
	ValueFrom *struct {
		SecretKeyRef *struct {
			Name string `yaml:"name"`
			Key  string `yaml:"key"`
		} `yaml:"secretKeyRef"`
	} `yaml:"valueFrom"`
}

// renderedChartObjects returns the env of the Versus container and the data
// of every ConfigMap and Secret in one scenario's render.
func renderedChartObjects(t *testing.T, scenario string) (env []renderedEnvVar, configMaps, secrets []map[string]string) {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(chartRender(t, scenario)))
	for {
		var doc struct {
			Kind       string            `yaml:"kind"`
			Data       map[string]string `yaml:"data"`
			StringData map[string]string `yaml:"stringData"`
			Spec       struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Name string           `yaml:"name"`
							Env  []renderedEnvVar `yaml:"env"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := dec.Decode(&doc); err != nil {
			break
		}
		switch doc.Kind {
		case "ConfigMap":
			configMaps = append(configMaps, doc.Data)
		case "Secret":
			secrets = append(secrets, doc.Data, doc.StringData)
		case "Deployment":
			for _, container := range doc.Spec.Template.Spec.Containers {
				if container.Name == "versus-incident" {
					env = append(env, container.Env...)
				}
			}
		}
	}
	if len(configMaps) == 0 {
		t.Fatalf("scenario %q rendered no ConfigMap", scenario)
	}
	return env, configMaps, secrets
}

// TestHelmChartAWSCredentialsFromSecret proves the AWS keys reach the Versus
// container only as env read from the operator's Secret: each key name comes
// from a secretKeyRef, no ConfigMap or chart Secret mentions them, and the
// account set through --set loads as one default_chain account. The scenarios
// are helm/versus-incident/tests/108 and 110.
func TestHelmChartAWSCredentialsFromSecret(t *testing.T) {
	for _, test := range []struct {
		scenario, secret, idKey, secretKey, accountID string
		regions                                       []string
		costExplorer                                  bool
	}{
		{"108-aws-quickstart", "versus-aws", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "111122223333", []string{"us-east-1"}, true},
		{"110-aws-credentials-custom-keys", "team-aws-keys", "access-key-id", "secret-access-key", "444455556666", nil, false},
	} {
		t.Run(test.scenario, func(t *testing.T) {
			env, configMaps, secrets := renderedChartObjects(t, test.scenario)
			want := map[string]string{"AWS_ACCESS_KEY_ID": test.idKey, "AWS_SECRET_ACCESS_KEY": test.secretKey}
			found := map[string]int{}
			for _, entry := range env {
				key, ok := want[entry.Name]
				if !ok {
					continue
				}
				found[entry.Name]++
				if entry.Value != nil || entry.ValueFrom == nil || entry.ValueFrom.SecretKeyRef == nil ||
					entry.ValueFrom.SecretKeyRef.Name != test.secret || entry.ValueFrom.SecretKeyRef.Key != key {
					t.Fatalf("%s is not read from Secret %q key %q", entry.Name, test.secret, key)
				}
			}
			for name := range want {
				if found[name] != 1 {
					t.Fatalf("%s set %d times on the container, want once", name, found[name])
				}
			}
			for _, data := range configMaps {
				for file, body := range data {
					for _, text := range []string{test.secret, "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", test.idKey, test.secretKey} {
						if strings.Contains(body, text) {
							t.Fatalf("ConfigMap file %s mentions %q", file, text)
						}
					}
				}
			}
			for _, data := range secrets {
				for key := range data {
					if strings.Contains(strings.ToLower(key), "aws_") {
						t.Fatalf("chart Secret holds %q; the keys belong in the operator's Secret only", key)
					}
				}
			}

			loaded, err := loadConfigFromPath(writeRenderedConfig(t, renderChartFiles(t, test.scenario)))
			if err != nil {
				t.Fatal(err)
			}
			aws := loaded.Connectors.AWS
			if !aws.Enable || len(aws.Accounts) != 1 || aws.CostDataEnabled() != test.costExplorer {
				t.Fatalf("aws connector = %+v", aws)
			}
			account := aws.Accounts[0]
			if account.AccountID != test.accountID || account.Auth.Mode != "default_chain" || len(account.Regions) != len(test.regions) ||
				len(test.regions) > 0 && !reflect.DeepEqual(account.Regions, test.regions) {
				t.Fatalf("account = %+v", account)
			}
		})
	}
}
