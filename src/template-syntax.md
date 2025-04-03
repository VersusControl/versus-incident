# Template Syntax Guide

This document explains the template syntax (Go template syntax) used for create a custom alert template in Versus Incident.

## Table of Contents
- [Basic Syntax](#basic-syntax)
  - [Access Data](#access-data)
  - [Variables](#variables)
  - [Pipelines](#pipelines)
- [Control Structures](#control-structures)
  - [Conditionals (if/else)](#conditionals)
  - [Loops (range)](#loops)


## Basic Syntax

### Access Data

Access data fields using double curly braces and dot notation, for example, with the data:

```json
{
  "Logs": "[ERROR] This is an error log from User Service that we can obtain using Fluent Bit.",
  "ServiceName": "order-service",
}
```

Example template:
```
*Error in {{ .ServiceName }}*
{{ .Logs }}
```

### Variables
You can declare variables within a template using the {{ $variable := value }} syntax. Once declared, variables can be used throughout the template, for example:

```
{{ $owner := "Team Alpha" }}
Owner: {{ $owner }}
```

Output:
```
Owner: Team Alpha
```

### Pipelines
Pipelines allow you to chain together multiple actions or functions. The result of one action can be passed as input to another, for example:

**upper**: Converts a string to uppercase.

```
*{{ .ServiceName | upper }} Failure*
```

**lower**: Converts a string to lowercase.

```
*{{ .ServiceName | lower }} Failure*
```

**title**: Converts a string to title case (first letter of each word capitalized).

```
*{{ .ServiceName | title }} Failure*
```

**default**: Provides a default value if the input is empty.

```
*{{ .ServiceName | default "unknown-service" }} Failure*
```

**slice**: Extracts a sub-slice from a slice or string.

```
{{ .Logs | slice 0 50 }}  // First 50 characters
```

**replace**: Replaces occurrences of a substring.

```
{{ .Logs | replace "error" "issue" }}
```

**trimPrefix**: Trims a prefix from a string.

```
{{ .Logs | trimPrefix "prod-" }}
```

**trimSuffix**: Trims a suffix from a string.

```
{{ .Logs | trimSuffix "-service" }}
```

**len**: Returns the length

```
{{ .Logs | len }}  // Length of the message
```

**urlquery**: Escapes a string for use in a URL query.

```
uri /search?q={{ .Query | urlquery }}
```

**split**: splits a string into array using a separator.

```
{{ $parts := split "apple,banana,cherry" "," }}

{{/* Iterate over split results */}}
{{ range $parts }}
  {{ . }}
{{ end }}
```

**You can chain multiple pipes together**:

```
{{ .Logs | trim | lower | truncate 50 }}
```

## Control Structures

### Conditionals

The templates support conditional logic using if, else, and end keywords.

```
{{ if .IsCritical }}
🚨 CRITICAL ALERT 🚨
{{ else }}
⚠️ Warning Alert ⚠️
{{ end }}
```

and:

```
{{ and .Value1 .Value2 .Value3 }}
```

or:

```
{{ or .Value1 .Value2 "default" }}
```

**Best Practices**

Error Handling:

```
{{ If .Error }}
  {{ .Details }}
{{ else }}
  No error details
{{ end }}
```

Whitespace Control:

```
{{- if .Production }}  // Remove preceding whitespace
PROD ALERT{{ end -}}   // Remove trailing whitespace
```

Template Comments:

```
{{/* This is a hidden comment */}}
```

Negates a boolean value:

```
{{ if not .IsCritical }}
  This is not a critical issue.
{{ end }}
```

Checks if two values are equal:

```
{{ if eq .Status "critical" }}
  🚨 Critical Alert 🚨
{{ end }}
```

Checks if two values are not equal:

```
{{ if ne .Env "production" }}
  This is not a production environment.
{{ end }}
```

Returns the length of a string, slice, array, or map:

```
{{ if gt (len .Errors) 0 }}
  There are {{ len .Errors }} errors.
{{ end }}
```

Checks if a string has a specific prefix:

```
{{ if .ServiceName | hasPrefix "prod-" }}
  Production service!
{{ end }}
```

Checks if a string has a specific suffix:

```
{{ if .ServiceName | hasSuffix "-service" }}
  This is a service.
{{ end }}
```

Checks if a message contains a specific strings:

```
{{ if contains .Logs "error" }}
  The message contains error logs.
{{ else }}
  The message does NOT contain error.
{{ end }}
```

### Loops

Iterate over slices/arrays with range:

```
{{ range .ErrorStack }}
- {{ . }}
{{ end }}
```