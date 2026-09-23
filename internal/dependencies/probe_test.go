package dependencies_test

import "testing"

// A throwaway branch: this failure gives pr-mon a failed GitHub Actions run to
// re-run while its draft toggle and re-run action are tested.
func TestDeliberateFailure(t *testing.T) {
	t.Fatal("deliberate failure")
}
