package calleridentity

import "testing"

func TestModelPathsAreNotTelemetry(t *testing.T) {
	for _, v := range []string{"/private/models/local", "C:/models/local", "https://host/model", "./models/local", "../model", "group/../private"} {
		if m := Model(v); m != "" {
			t.Fatalf("stored a path as a model: %q", m)
		}
	}
	if m := Model("provider/model:latest"); m != "provider/model:latest" {
		t.Fatal("lost provider-qualified model")
	}
}
