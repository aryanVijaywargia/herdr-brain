package paths

import (
	"fmt"
	"os"
)

// Temporary lint probe. Expect errcheck on the Fprintf, the Close and the bare defer Close.
func lintProbeWrite(name string) {
	f, err := os.Create(name)
	if err != nil {
		return
	}
	fmt.Fprintf(f, "x\n")
	f.Close()
}

// Expect no errcheck finding: the nolint form from rule 17.
func lintProbeRead(name string) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read-only: Close cannot lose data
	return nil
}

// Expect errcheck: a bare defer Close.
func lintProbeReadBare(name string) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	return nil
}
