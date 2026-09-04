package main

import "github.com/flaviopadilha/device-farmer/migrations"

// A //go:embed pattern may not contain ".." and may not leave the embedding
// package's directory, so cmd/farmd cannot embed ../../migrations itself. The
// migrations package embeds its own .sql files and this file adopts them,
// which is the whole reason migrationsFS is a variable rather than a
// //go:embed declaration in migrate.go.
//
// The effect is that "farmd migrate up" carries the schema inside the binary.
// An image that shipped the binary without the SQL would otherwise migrate to
// version 0 and report success, leaving a running control plane pointed at an
// empty database.
func init() { migrationsFS = migrations.FS }
