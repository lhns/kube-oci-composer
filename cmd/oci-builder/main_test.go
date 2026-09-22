package main

import (
	"reflect"
	"testing"
)

// TestEveryJobFlagIsWired: every jobFlags field must reach JobConfig (--context-base-url once did
// not, and only the e2e noticed). Reflection, so a newly added field is covered automatically.
func TestEveryJobFlagIsWired(t *testing.T) {
	// Every field set to something distinguishable from its zero value.
	in := jobFlags{
		BuilderImage:       "builder@sha256:a",
		FrontendImage:      "frontend@sha256:b",
		FetcherImage:       "fetcher@sha256:c",
		ContextBaseURL:     "http://ctx:8090",
		SourceDateEpoch:    "1700000000",
		InsecureRegistries: []string{"registry.internal"},
		RegistryCA:         []byte("-----BEGIN CERTIFICATE-----"),
		SBOM:               true,
		Provenance:         true,
	}

	// A field left zero here would go unchecked.
	flags := reflect.ValueOf(in)
	for i := range flags.NumField() {
		if flags.Field(i).IsZero() {
			t.Fatalf("jobFlags.%s is not set in this fixture, so nothing here proves it is wired",
				flags.Type().Field(i).Name)
		}
	}

	got := reflect.ValueOf(newJobConfig(in))
	for i := range flags.NumField() {
		name := flags.Type().Field(i).Name
		field := got.FieldByName(name)
		if !field.IsValid() {
			t.Errorf("JobConfig has no field %s; jobFlags carries one nothing reads", name)
			continue
		}
		if field.IsZero() {
			t.Errorf("JobConfig.%s is zero: the flag exists but never reaches the controller", name)
		}
	}
}
