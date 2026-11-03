//go:build !darwin && !linux

package storage

import "os"

func validateLocalEvidenceArtifact(string) error             { return ErrUnsupported }
func prepareLocalEvidenceArtifact(string) error              { return ErrUnsupported }
func listLegacyLocalEvidence(string) ([]os.DirEntry, error)  { return nil, ErrUnsupported }
func readLegacyLocalEvidence(string, string) ([]byte, error) { return nil, ErrUnsupported }
