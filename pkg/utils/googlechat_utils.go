package utils

// GoogleChatCardMessage represents the structure of a Google Chat card message
type GoogleChatCardMessage struct {
	CardsV2 []GoogleChatCard `json:"cardsV2"`
}

// GoogleChatCard represents a card in Google Chat
type GoogleChatCard struct {
	GoogleChatCardID string             `json:"cardId"`
	GoogleChatCard   GoogleChatCardData `json:"card"`
}

// GoogleChatCardData represents the data of a Google Chat card
type GoogleChatCardData struct {
	Header   GoogleChatCardHeader    `json:"header"`
	Sections []GoogleChatCardSection `json:"sections"`
}

// GoogleChatCardHeader represents the header of a Google Chat card
type GoogleChatCardHeader struct {
	Title    string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"`
}

// GoogleChatCardSection represents a section in a Google Chat card
type GoogleChatCardSection struct {
	Widgets []GoogleChatCardWidgets `json:"widgets"`
}

// GoogleChatCardWidget represents a widget in a Google Chat card section
type GoogleChatCardWidgets struct {
	TextParagraph *GoogleChatTextParagraph `json:"textParagraph,omitempty"`
	ButtonList    *ButtonList              `json:"buttonList,omitempty"`
}

// GoogleChatTextParagraph represents a text paragraph widget in Google Chat
type GoogleChatTextParagraph struct {
	Text string `json:"text"`
}

type ButtonList struct {
	Buttons []Button `json:"buttons"`
}

type Button struct {
	Text    string  `json:"text"`
	OnClick OnClick `json:"onClick"`
}

type OnClick struct {
	OpenLink *OpenLink `json:"openLink,omitempty"`
}

type OpenLink struct {
	URL string `json:"url"`
}

func CreateGoogleChatMessage(content string, buttonMap map[string]string, displayButtons []string, isResolved bool) *GoogleChatCardMessage {
	title := "Alert"

	// Add status indicator to title
	if isResolved {
		title = "✅ Resolved " + title
	} else {
		title = "🚨 Firing " + title
	}

	// Create buttons from displayButtons list using buttonMap
	var buttons []Button
	for _, buttonName := range displayButtons {
		if url, exists := buttonMap[buttonName]; exists {
			buttons = append(buttons, Button{
				Text: buttonName,
				OnClick: OnClick{
					OpenLink: &OpenLink{URL: url},
				},
			})
		}
	}

	// Build widgets - start with text paragraph
	widgets := []GoogleChatCardWidgets{
		{TextParagraph: &GoogleChatTextParagraph{Text: content}},
	}

	// Add button list widget only if there are buttons
	if len(buttons) > 0 {
		widgets = append(widgets, GoogleChatCardWidgets{
			ButtonList: &ButtonList{Buttons: buttons},
		})
	}

	// Create the card message
	return &GoogleChatCardMessage{
		CardsV2: []GoogleChatCard{
			{
				GoogleChatCardID: "alert-card",
				GoogleChatCard: GoogleChatCardData{
					Header: GoogleChatCardHeader{
						Title:    title,
						Subtitle: "",
					},
					Sections: []GoogleChatCardSection{
						{
							Widgets: widgets,
						},
					},
				},
			},
		},
	}
}
