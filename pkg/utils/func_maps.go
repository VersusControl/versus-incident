// Package utils provides utility functions for the application
package utils

import (
	"bytes"
	"fmt"
	"html/template"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// TemplateIncludeStore provides a mechanism for template includes to work
var (
	templateRegistry     = make(map[string]*template.Template)
	templateRegistryLock sync.RWMutex
)

// TemplateDict is a custom dictionary type that supports the Set method
type TemplateDict map[string]interface{}

// Set adds or updates a key-value pair in the dictionary
func (d TemplateDict) Set(key string, value interface{}) TemplateDict {
	d[key] = value
	return d
}

// RegisterTemplate registers a template for use with the include function
func RegisterTemplate(name string, tmpl *template.Template) {
	templateRegistryLock.Lock()
	defer templateRegistryLock.Unlock()
	templateRegistry[name] = tmpl
}

// GetIncludeFunc returns an include function that uses the specified template
func GetIncludeFunc(tmplName string) func(string, interface{}) (string, error) {
	return func(name string, data interface{}) (string, error) {
		templateRegistryLock.RLock()
		tmpl, ok := templateRegistry[tmplName]
		templateRegistryLock.RUnlock()

		if !ok {
			return "", fmt.Errorf("template %s not registered for include function", tmplName)
		}

		var buf bytes.Buffer
		err := tmpl.ExecuteTemplate(&buf, name, data)
		if err != nil {
			return "", err
		}
		return buf.String(), nil
	}
}

// GetTemplateFuncMaps returns a map of template functions that can be used in templates
func GetTemplateFuncMaps() template.FuncMap {
	funcMaps := template.FuncMap{
		// String manipulation functions
		"replaceAll": strings.ReplaceAll,
		"contains":   strings.Contains,
		"upper":      strings.ToUpper,
		"lower":      strings.ToLower,
		"title": func(s string) string {
			return cases.Title(language.English).String(s)
		},
		"toString": func(v interface{}) string {
			return fmt.Sprintf("%v", v)
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
		"join":       strings.Join,
		"stringSlice": func(interfaces []interface{}) []string {
			result := make([]string, len(interfaces))
			for i, v := range interfaces {
				result[i] = fmt.Sprint(v)
			}
			return result
		},
		"len": func(s interface{}) int {
			// Return length of string or slice
			switch v := s.(type) {
			case string:
				return len(v)
			case []interface{}:
				return len(v)
			case []string:
				return len(v)
			case map[string]interface{}:
				return len(v)
			case TemplateDict:
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

		// Environment and configuration
		"env": func(key string) string {
			return os.Getenv(key)
		},

		// Data structure functions
		"dict": func(values ...interface{}) (TemplateDict, error) {
			if len(values)%2 != 0 {
				return nil, fmt.Errorf("dict expects an even number of arguments")
			}
			dict := make(TemplateDict, len(values)/2)
			for i := 0; i < len(values); i += 2 {
				key, ok := values[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict keys must be strings")
				}
				dict[key] = values[i+1]
			}
			return dict, nil
		},
		"list": func(items ...interface{}) []interface{} {
			return items
		},
		"append": func(slice []interface{}, items ...interface{}) []interface{} {
			return append(slice, items...)
		},

		// Map operations
		"index": func(obj interface{}, key interface{}) interface{} {
			// Handle nil and empty values
			if obj == nil {
				return nil
			}

			v := reflect.ValueOf(obj)
			// Handle different types
			switch v.Kind() {
			case reflect.Map:
				// For maps, convert the key to the proper type
				keyValue := reflect.ValueOf(key)
				if !keyValue.Type().AssignableTo(v.Type().Key()) {
					// If key type doesn't match, try to convert common types
					if v.Type().Key().Kind() == reflect.String && keyValue.Type().Kind() != reflect.String {
						// Try to convert key to string
						keyStr := fmt.Sprintf("%v", key)
						keyValue = reflect.ValueOf(keyStr)
					}
				}

				if keyValue.Type().AssignableTo(v.Type().Key()) {
					value := v.MapIndex(keyValue)
					if value.IsValid() {
						return value.Interface()
					}
				}
				return nil
			case reflect.Slice, reflect.Array:
				// For slices and arrays, key must be an integer
				switch k := key.(type) {
				case int:
					if k >= 0 && k < v.Len() {
						return v.Index(k).Interface()
					}
				case int64:
					idx := int(k)
					if idx >= 0 && idx < v.Len() {
						return v.Index(idx).Interface()
					}
				case float64:
					idx := int(k)
					if idx >= 0 && idx < v.Len() {
						return v.Index(idx).Interface()
					}
				}
				return nil
			case reflect.Struct:
				// Handle struct field access by string name
				if keyStr, ok := key.(string); ok {
					field := v.FieldByName(keyStr)
					if field.IsValid() {
						return field.Interface()
					}
				}
				return nil
			default:
				return nil
			}
		},

		// Date/time functions
		"now": func() time.Time {
			return time.Now()
		},
		"format": func(layout string, t interface{}) string {
			switch v := t.(type) {
			case time.Time:
				return v.Format(layout)
			case string:
				// Try to parse the string as a time
				parsedTime, err := time.Parse(time.RFC3339, v)
				if err != nil {
					// Try other common formats
					formats := []string{
						time.RFC3339,
						time.RFC3339Nano,
						time.RFC1123,
						time.RFC1123Z,
						time.RFC822,
						time.RFC822Z,
						time.RFC850,
						"2006-01-02T15:04:05",
						"2006-01-02 15:04:05",
						"2006/01/02 15:04:05",
						"2006-01-02",
						"01/02/2006",
					}

					for _, f := range formats {
						parsedTime, err = time.Parse(f, v)
						if err == nil {
							break
						}
					}

					if err != nil {
						return "Invalid time format"
					}
				}
				return parsedTime.Format(layout)
			default:
				return fmt.Sprintf("Unsupported type for format: %T", t)
			}
		},

		// String formatting
		"printf": fmt.Sprintf,

		// Comparison operators
		"eq": func(a, b interface{}) bool {
			return reflect.DeepEqual(a, b)
		},
		"ne": func(a, b interface{}) bool {
			return !reflect.DeepEqual(a, b)
		},
		"lt": func(a, b interface{}) bool {
			switch a := a.(type) {
			case int:
				if b, ok := b.(int); ok {
					return a < b
				}
			case string:
				if b, ok := b.(string); ok {
					return a < b
				}
			}
			return false
		},
		"gt": func(a, b interface{}) bool {
			switch a := a.(type) {
			case int:
				if b, ok := b.(int); ok {
					return a > b
				}
			case string:
				if b, ok := b.(string); ok {
					return a > b
				}
			}
			return false
		},

		// Logical operators
		"and": func(args ...interface{}) bool {
			for _, arg := range args {
				if !isTrue(arg) {
					return false
				}
			}
			return true
		},
		"or": func(args ...interface{}) interface{} {
			for _, arg := range args {
				if isTrue(arg) {
					return arg
				}
			}
			return args[len(args)-1]
		},
		"not": func(arg interface{}) bool {
			return !isTrue(arg)
		},

		// Math functions
		"add": func(a, b interface{}) interface{} {
			// Handle different numeric types
			switch a := a.(type) {
			case int:
				switch b := b.(type) {
				case int:
					return a + b
				case int64:
					return int64(a) + b
				case float64:
					return float64(a) + b
				}
			case int64:
				switch b := b.(type) {
				case int:
					return a + int64(b)
				case int64:
					return a + b
				case float64:
					return float64(a) + b
				}
			case float64:
				switch b := b.(type) {
				case int:
					return a + float64(b)
				case int64:
					return a + float64(b)
				case float64:
					return a + b
				}
			}
			// If we can't add numerically, fall back to string concatenation
			return fmt.Sprintf("%v%v", a, b)
		},

		// Template inclusion function that works with defined templates
		"include": func(name string, data interface{}) (string, error) {
			if data == nil {
				data = struct{}{}
			}

			// This is a special function that will be overridden at runtime
			// when a template is actually being executed. This stub implementation
			// is just for template parsing.
			return "", nil
		},

		// Regex match function
		"regexMatch": func(pattern, s string) bool {
			match, _ := regexp.MatchString(pattern, s)
			return match
		},
	}

	return funcMaps
}

// isTrue reports whether the value is 'true', in the sense of not the zero of its type,
// and whether the value has a meaningful truth value. This is the same as in Go's if
// and other conditional constructs.
func isTrue(val interface{}) bool {
	if val == nil {
		return false
	}

	v := reflect.ValueOf(val)
	switch v.Kind() {
	case reflect.Bool:
		return v.Bool()
	case reflect.String:
		return v.String() != ""
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() != 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint() != 0
	case reflect.Float32, reflect.Float64:
		return v.Float() != 0
	case reflect.Map, reflect.Slice, reflect.Array:
		return v.Len() > 0
	case reflect.Struct:
		return true // Always consider structs to be true
	case reflect.Ptr, reflect.Interface:
		return !v.IsNil()
	default:
		return false
	}
}
