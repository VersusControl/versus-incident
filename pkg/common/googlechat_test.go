package common

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	m "github.com/VersusControl/versus-incident/pkg/models"
	"github.com/VersusControl/versus-incident/pkg/utils" // Assuming GetTemplateFuncMaps is here
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testTemplateDir          = "../../config" // Relative path to templates from this test file
	googleChatTestTemplate   = "googlechat_message.tmpl"
	validTestTemplatePath    = "../../config/googlechat_message.tmpl"
	invalidTestTemplatePath  = "../../config/nonexistent_template.tmpl"
	mockAckURL               = "http://localhost/ack/123"
	mockButtonText           = "Acknowledge Test"
)

// Helper to create a temporary template file for testing specific template content or errors
func createTempTemplateFile(t *testing.T, content string) string {
	t.Helper()
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "test_googlechat_message.tmpl")
	err := ioutil.WriteFile(tmpFile, []byte(content), 0644)
	require.NoError(t, err, "Failed to create temp template file")
	return tmpFile
}

// setupGoogleChatProvider is a helper function to create a GoogleChatProvider for tests
func setupGoogleChatProvider(t *testing.T, webhookURL, templatePath, buttonText string) *GoogleChatProvider {
	t.Helper()
	cfg := config.GoogleChatConfig{
		WebhookURL:   webhookURL,
		TemplatePath: templatePath,
		MessageProperties: config.GoogleChatMessageProperties{
			ButtonText: buttonText,
		},
	}
	// Create the directory if it doesn't exist, for template path validation
	if templatePath != "" && !strings.Contains(templatePath, "nonexistent") {
		err := os.MkdirAll(filepath.Dir(templatePath), 0755)
		if err != nil && !os.IsExist(err) {
			t.Fatalf("Failed to create directory for template: %v", err)
		}
		// Create a dummy template file if it doesn't exist, to avoid parsing errors for valid path tests
		if _, err := os.Stat(templatePath); os.IsNotExist(err) {
			if strings.HasSuffix(templatePath, googleChatTestTemplate) { // Only create if it's the actual template
				// Attempt to copy the real template to the test location if running from a context where it's available
				// This is a bit fragile; ideally, tests run from project root or templates are embedded.
				originalTemplatePath := filepath.Join(testTemplateDir, googleChatTestTemplate)
				content, err := ioutil.ReadFile(originalTemplatePath)
				if err == nil {
					err = ioutil.WriteFile(templatePath, content, 0644)
					require.NoError(t, err, "Failed to copy real template to test location")
				} else {
					// Fallback: create a minimal valid template if real one not found
					minimalContent := `{ "cardsV2": [{ "cardId": "test", "card": { "header": { "title": "{{ .Incident.title }}" } } }] }`
					err = ioutil.WriteFile(templatePath, []byte(minimalContent), 0644)
					require.NoError(t, err, "Failed to create dummy template file")
				}
			}
		}
	}
	return NewGoogleChatProvider(cfg)
}

func TestNewGoogleChatProvider(t *testing.T) {
	cfg := config.GoogleChatConfig{
		WebhookURL:   "http://fake.webhook.url",
		TemplatePath: "path/to/template.tmpl",
		MessageProperties: config.GoogleChatMessageProperties{
			ButtonText: "Test Button",
		},
	}
	provider := NewGoogleChatProvider(cfg)

	assert.NotNil(t, provider)
	assert.Equal(t, cfg.WebhookURL, provider.webhookURL)
	assert.Equal(t, cfg.TemplatePath, provider.templatePath)
	assert.Equal(t, cfg.MessageProperties.ButtonText, provider.msgProps.ButtonText)
	assert.NotNil(t, provider.httpClient)
}

// GoogleChatCard represents the structure of the JSON payload sent to Google Chat
// Simplified for testing key elements.
type GoogleChatCard struct {
	CardsV2 []struct {
		CardID string `json:"cardId"`
		Card   struct {
			Header struct {
				Title    string `json:"title"`
				Subtitle string `json:"subtitle"`
			} `json:"header"`
			Sections []struct {
				Widgets []struct {
					TextParagraph *struct {
						Text string `json:"text"`
					} `json:"textParagraph,omitempty"`
					ButtonList *struct {
						Buttons []struct {
							Text     string `json:"text"`
							OnClick struct {
								OpenLink struct {
									URL string `json:"url"`
								} `json:"openLink"`
							} `json:"onClick"`
						} `json:"buttons"`
					} `json:"buttonList,omitempty"`
				} `json:"widgets"`
			} `json:"sections"`
		} `json:"card"`
	} `json:"cardsV2"`
}

func TestGoogleChatProvider_SendAlert_Unresolved(t *testing.T) {
	// Ensure template functions are available (mock or real)
	// If utils.GetTemplateFuncMaps() is not fully implemented, this test might fail at template execution.
	utils.InitFuncMaps() // Assuming this initializes the maps needed by the template

	// Create a temporary valid template file for this test
	// This ensures the test doesn't depend on the global template file's exact content,
	// making it more robust for testing SendAlert logic itself.
	// The actual template rendering is complex, so we use a simplified one or the real one if available.
	
	// Try to use the real template, copy it to a temp location for this test
	tempTemplateDir := t.TempDir()
	tempTemplatePath := filepath.Join(tempTemplateDir, googleChatTestTemplate)

	originalTemplateFile := filepath.Join(testTemplateDir, googleChatTestTemplate)
	originalContent, err := ioutil.ReadFile(originalTemplateFile)
	if err != nil {
		t.Logf("Could not read original template '%s', using minimal: %v", originalTemplateFile, err)
		// Fallback to a minimal template if the main one isn't accessible from test context
		minimalTemplate := `{
			"cardsV2": [{
				"cardId": "alertCard-{{ now.UnixNano }}",
				"card": {
					"header": { "title": "{{ $data := .Incident }}{{ $statusIcon := \"🔥\"}}{{ $finalStatus := $data.status | upper }}{{ printf \"%s %s: %s\" $statusIcon $finalStatus ($data.title | default \"Incident\") }}" },
					"sections": [
						{ "widgets": [ { "textParagraph": { "text": "Desc: {{ .Incident.description }}" } } ] }
						{{ if and .AckURL (not .IsResolved) }}
						,{ "widgets": [ { "buttonList": { "buttons": [ { "text": "{{ .ButtonText }}", "onClick": { "openLink": { "url": "{{ .AckURL }}" } } } ] } } ] }
						{{ end }}
					]
				}
			}]
		}`
		err = ioutil.WriteFile(tempTemplatePath, []byte(minimalTemplate), 0644)
		require.NoError(t, err)
	} else {
		err = ioutil.WriteFile(tempTemplatePath, originalContent, 0644)
		require.NoError(t, err)
	}


	var receivedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		body, err := ioutil.ReadAll(r.Body)
		require.NoError(t, err)
		receivedBody = body
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"status":"success"}`) // Mock Google Chat API response
	}))
	defer server.Close()

	provider := setupGoogleChatProvider(t, server.URL, tempTemplatePath, mockButtonText)

	incidentContent := map[string]interface{}{
		"title":       "Test Unresolved Alert",
		"description": "This is a test description for an unresolved alert.",
		"status":      "firing",
		"severity":    "critical",
		"ackurl":      mockAckURL, // Ensure ackurl is present
		"commonAnnotations": map[string]string{"summary": "Test Unresolved Alert"},
		"commonLabels": map[string]string{"severity": "critical"},
		"startsAt": time.Now().Format(time.RFC3339),
	}
	incident := &m.Incident{
		ID:      "incident-123",
		Source:  "test",
		Content: &incidentContent,
	}

	err = provider.SendAlert(incident)
	assert.NoError(t, err)
	require.NotNil(t, receivedBody, "Request body should not be nil")

	var cardPayload GoogleChatCard
	err = json.Unmarshal(receivedBody, &cardPayload)
	require.NoError(t, err, "Failed to unmarshal request body. Body: %s", string(receivedBody))

	require.Len(t, cardPayload.CardsV2, 1, "Should have one card in cardsV2")
	card := cardPayload.CardsV2[0].Card
	assert.Contains(t, card.Header.Title, "FIRING: Test Unresolved Alert", "Card header title mismatch")

	// Check for description (example, depends on template structure)
	foundDescription := false
	foundButton := false
	for _, section := range card.Sections {
		for _, widget := range section.Widgets {
			if widget.TextParagraph != nil && strings.Contains(widget.TextParagraph.Text, "This is a test description") {
				foundDescription = true
			}
			if widget.ButtonList != nil {
				require.Len(t, widget.ButtonList.Buttons, 1, "Should have one button")
				button := widget.ButtonList.Buttons[0]
				assert.Equal(t, mockButtonText, button.Text)
				assert.Equal(t, mockAckURL, button.OnClick.OpenLink.URL)
				foundButton = true
			}
		}
	}
	assert.True(t, foundDescription, "Description not found in card widgets")
	assert.True(t, foundButton, "Acknowledge button not found or incorrect")
}


func TestGoogleChatProvider_SendAlert_Resolved(t *testing.T) {
	utils.InitFuncMaps()
	tempTemplateDir := t.TempDir()
	tempTemplatePath := filepath.Join(tempTemplateDir, googleChatTestTemplate)
	originalTemplateFile := filepath.Join(testTemplateDir, googleChatTestTemplate)
	originalContent, err := ioutil.ReadFile(originalTemplateFile)
	if err != nil {
		t.Logf("Could not read original template '%s', using minimal: %v", originalTemplateFile, err)
		minimalTemplate := `{
			"cardsV2": [{
				"cardId": "alertCard-{{ now.UnixNano }}",
				"card": {
					"header": { "title": "{{ $data := .Incident }}{{ $statusIcon := \"✅\"}}{{ $finalStatus := $data.status | upper }}{{ if .IsResolved }}RESOLVED{{ else }}{{ $finalStatus }}{{ end }}: {{ $data.title | default \"Incident\" }}" },
					"sections": [ { "widgets": [ { "textParagraph": { "text": "Desc: {{ .Incident.description }}" } } ] } ]
				}
			}]
		}`
		err = ioutil.WriteFile(tempTemplatePath, []byte(minimalTemplate), 0644)
		require.NoError(t, err)
	} else {
		err = ioutil.WriteFile(tempTemplatePath, originalContent, 0644)
		require.NoError(t, err)
	}
	
	var receivedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := ioutil.ReadAll(r.Body)
		receivedBody = body
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	provider := setupGoogleChatProvider(t, server.URL, tempTemplatePath, mockButtonText)

	incidentContent := map[string]interface{}{
		"title":       "Test Resolved Alert",
		"description": "This is a test description for a resolved alert.",
		"status":      "resolved",
		"severity":    "critical",
		"commonAnnotations": map[string]string{"summary": "Test Resolved Alert"},
		"commonLabels": map[string]string{"severity": "critical"},
		"endsAt": time.Now().Format(time.RFC3339),
	}
	incident := &m.Incident{ID: "incident-456", Content: &incidentContent}

	err = provider.SendAlert(incident)
	assert.NoError(t, err)

	var cardPayload GoogleChatCard
	err = json.Unmarshal(receivedBody, &cardPayload)
	require.NoError(t, err, "Failed to unmarshal. Body: %s", string(receivedBody))

	require.Len(t, cardPayload.CardsV2, 1)
	card := cardPayload.CardsV2[0].Card
	assert.Contains(t, card.Header.Title, "RESOLVED: Test Resolved Alert", "Card header title mismatch for resolved alert")

	buttonFound := false
	for _, section := range card.Sections {
		for _, widget := range section.Widgets {
			if widget.ButtonList != nil && len(widget.ButtonList.Buttons) > 0 {
				buttonFound = true
				break
			}
		}
		if buttonFound { break }
	}
	assert.False(t, buttonFound, "Acknowledge button should not be present for resolved alerts")
}

func TestGoogleChatProvider_SendAlert_HTTPError(t *testing.T) {
	utils.InitFuncMaps()
	tempTemplateDir := t.TempDir()
	tempTemplatePath := filepath.Join(tempTemplateDir, googleChatTestTemplate)
	originalTemplateFile := filepath.Join(testTemplateDir, googleChatTestTemplate)
	originalContent, err := ioutil.ReadFile(originalTemplateFile)
	if err != nil {
		t.Logf("Could not read original template '%s', using minimal: %v", originalTemplateFile, err)
		minimalTemplate := `{ "cardsV2": [{ "cardId": "test", "card": { "header": { "title": "Test" } } }] }`
		err = ioutil.WriteFile(tempTemplatePath, []byte(minimalTemplate), 0644)
		require.NoError(t, err)
	} else {
		err = ioutil.WriteFile(tempTemplatePath, originalContent, 0644)
		require.NoError(t, err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	provider := setupGoogleChatProvider(t, server.URL, tempTemplatePath, mockButtonText)
	incidentContent := map[string]interface{}{"title": "HTTP Error Test", "status": "firing"}
	incident := &m.Incident{ID: "incident-789", Content: &incidentContent}

	err = provider.SendAlert(incident)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to send Google Chat message: status code 500")
}

func TestGoogleChatProvider_SendAlert_TemplateError_NonExistentTemplate(t *testing.T) {
	provider := setupGoogleChatProvider(t, "http://dummyurl", invalidTestTemplatePath, mockButtonText)
	incidentContent := map[string]interface{}{"title": "Template Error Test", "status": "firing"}
	incident := &m.Incident{ID: "incident-000", Content: &incidentContent}

	err := provider.SendAlert(incident)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse Google Chat template")
}

func TestGoogleChatProvider_SendAlert_TemplateError_BadTemplate(t *testing.T) {
	utils.InitFuncMaps()
	// Create a temporary template file with invalid syntax
	badTemplateContent := `{ "cardsV2": [ { "cardId": "test", "card": { "header": { "title": "{{ .Incident.title" } } } ] }` // Missing closing }}
	tempTemplateFile := createTempTemplateFile(t, badTemplateContent)
	defer os.Remove(tempTemplateFile) // Clean up

	provider := setupGoogleChatProvider(t, "http://dummyurl", tempTemplateFile, mockButtonText)
	incidentContent := map[string]interface{}{"title": "Bad Template Test", "status": "firing"}
	incident := &m.Incident{ID: "incident-111", Content: &incidentContent}

	err := provider.SendAlert(incident)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to execute Google Chat template", "Error message should indicate template execution failure")
}

func TestGoogleChatProvider_SendAlert_MissingTemplatePath(t *testing.T) {
	provider := setupGoogleChatProvider(t, "http://dummyurl", "", mockButtonText) // Empty template path
	incidentContent := map[string]interface{}{"title": "Missing Path Test", "status": "firing"}
	incident := &m.Incident{ID: "incident-222", Content: &incidentContent}

	err := provider.SendAlert(incident)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "google chat template path is not configured")
}

// Mock utils.InitFuncMaps and utils.GetTemplateFuncMaps if they are not readily available
// or if you want to control them for tests.
// For now, we assume they exist and work as expected from pkg/utils.
// If not, you'd need to add something like this:
//
// package utils
//
// import "text/template"
//
// var funcMaps template.FuncMap
//
// func InitFuncMaps() {
//  funcMaps = template.FuncMap{
//    "now": func() time.Time { return time.Now() },
//    "toJson": func(v interface{}) string { /* basic json marshal */ return "" },
//    "escapeJsonString": func(s string) string { return s /* basic escape */ },
//    "newStringBuilder": func() *strings.Builder { return &strings.Builder{} },
// 		"newWidgetList": func() *widgetList { return &widgetList{} }, // Define widgetList or simplify
//    "upper": strings.ToUpper,
//    "replace": func(input, from, to string) string { return strings.ReplaceAll(input, from, to) },
//		"formatAsTime": func(t interface{}) string { return fmt.Sprintf("%v", t) }, // Simplified
//      // ... other functions used by the template
//  }
// }
//
// func GetTemplateFuncMaps() template.FuncMap {
//  if funcMaps == nil {
//    InitFuncMaps()
//  }
//  return funcMaps
// }
// type widgetList struct {
// 	items []string
// }
// func (wl *widgetList) Add(item string) {
// 	wl.items = append(wl.items, item)
// }
// func (wl *widgetList) Join(sep string) string {
// 	return strings.Join(wl.items, sep)
// }

// Ensure utils.Logger is usable in tests, e.g., by setting it to a test logger or ensuring it handles nil.
func init() {
	// This is a simple way to ensure utils.Logger is not nil.
	// In a real scenario, you might use a more sophisticated test logger.
	utils.InitLogger("test", "debug", "console", "")
}
