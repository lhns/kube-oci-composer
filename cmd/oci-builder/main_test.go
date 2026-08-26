package main

import (
	"reflect"
	"testing"
)

// TestEveryJobFlagIsWired closes a hole every other layer is blind to.
//
// --context-base-url was defined, documented, rendered by the chart and mounted into the Job's
// argv logic -- and never copied into the JobConfig the controller reads. So the whole feature was
// inert: builds kept fetching source-controller directly and the endpoint served nobody. Not one
// unit test noticed, because they all construct a JobConfig directly and never go through main;
// only the e2e caught it, twenty minutes at a time.
//
// Reflection over both structs rather than a hand-written list of fields, for the reason
// build.Inputs has the same guard: a list only covers what someone remembered to add, and the field
// that gets forgotten is exactly the one that was just introduced.
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

	// Anything not set above would make this test lie about what it covers.
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
