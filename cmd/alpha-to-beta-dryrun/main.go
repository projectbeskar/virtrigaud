package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintf(os.Stderr, "ERROR: v1alpha1 API has been removed from virtrigaud.\n")
	fmt.Fprintf(os.Stderr, "This migration tool is no longer needed as only v1beta1 is supported.\n")
	fmt.Fprintf(os.Stderr, "If you have existing v1alpha1 resources, they should have been migrated before this version.\n")
	os.Exit(1)
}
