// Package utils provides utility functions for the application
package utils

import (
	"html/template"
	"net/url"
	"strings"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// GetTemplateFuncMaps returns a map of template functions that can be used in templates
func GetTemplateFuncMaps() template.FuncMap {
	funcMaps := template.FuncMap{
		"replaceAll": strings.ReplaceAll,
		"contains":   strings.Contains,
		"upper":      strings.ToUpper,
		"lower":      strings.ToLower,
		"title": func(s string) string {
			return cases.Title(language.English).String(s)
		},
		"default": func(val, def interface{}) interface{} {
			// If val is nil or empty string, return def
			switch v := val.(type) {
			case string:
				if v == "" {
					return def
				}
			case nil:
				return def
			}
			return val
		},
		"slice": func(s interface{}, start, end int) string {
			// Handle string slicing
			switch v := s.(type) {
			case string:
				if start < 0 || end > len(v) || start > end {
					return v // Return original if indices are invalid
				}
				return v[start:end]
			}
			return "" // Return empty string for unsupported types
		},
		"replace":    strings.ReplaceAll,
		"trimPrefix": strings.TrimPrefix,
		"trimSuffix": strings.TrimSuffix,
		"split":      strings.Split,
		"len": func(s interface{}) int {
			// Return length of string or slice
			switch v := s.(type) {
			case string:
				return len(v)
			case []string:
				return len(v)
			}
			return 0
		},
		"urlquery": url.QueryEscape,
		"truncate": func(s string, n int) string {
			if len(s) <= n {
				return s
			}
			return s[:n]
		},
		"formatTime": func(s string) string {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return "Invalid time"
			}
			return t.Format("2006-01-02 15:04:05") // Formats as "YYYY-MM-DD HH:MM:SS"
		},
	}

	return funcMaps
}
