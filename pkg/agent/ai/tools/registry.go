// Package tools defines shared metadata for read-only AI tool catalogs.
package tools

import "github.com/VersusControl/versus-incident/pkg/core"

// Group identifies a domain-scoped toolset.
type Group string

const (
	GroupCommon Group = "common"
	GroupVersus Group = "versus"
	GroupK8s    Group = "k8s"
)

// Entry binds a tool to the domain group that owns it.
type Entry struct {
	Group Group
	Tool  core.Tool
}

// Registry is an ordered collection of grouped tools.
type Registry []Entry
