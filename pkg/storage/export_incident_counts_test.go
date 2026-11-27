package storage

import "time"

// SetIncidentCountTimeout shortens the database limit for incident counts on
// a Postgres provider, so an external test can watch a count reach it without
// waiting the full limit. It reports whether p is a Postgres provider.
func SetIncidentCountTimeout(p Provider, d time.Duration) bool {
	pg, ok := p.(*postgresProvider)
	if ok {
		pg.incidentCountTimeout = d
	}
	return ok
}

// SetIncidentListTimeout shortens the database limit for incident page reads
// on a Postgres provider. It reports whether p is a Postgres provider.
func SetIncidentListTimeout(p Provider, d time.Duration) bool {
	pg, ok := p.(*postgresProvider)
	if ok {
		pg.incidentListTimeout = d
	}
	return ok
}
