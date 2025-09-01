package roundtrip

import (
	"reflect"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/conversion"
)

// ConvertibleObject represents an object that can be converted
type ConvertibleObject interface {
	metav1.Object
	DeepCopyObject() runtime.Object
}

// Convertible represents a type that implements conversion
type Convertible interface {
	ConvertTo(dst conversion.Hub) error
	ConvertFrom(src conversion.Hub) error
}

// RoundTripTest performs round-trip conversion tests
func RoundTripTest(t *testing.T, original, hub ConvertibleObject) {
	t.Helper()

	// Test: Original -> Hub -> Original (alpha -> beta -> alpha)
	t.Run("Original_to_Hub_to_Original", func(t *testing.T) {
		testOriginalToHubToOriginal(t, original, hub)
	})

	// Test: Hub -> Original -> Hub (beta -> alpha -> beta)
	t.Run("Hub_to_Original_to_Hub", func(t *testing.T) {
		testHubToOriginalToHub(t, hub, original)
	})
}

func testOriginalToHubToOriginal(t *testing.T, original, hub ConvertibleObject) {
	t.Helper()

	// Create deep copies to avoid mutation
	originalCopy := original.DeepCopyObject().(ConvertibleObject) //nolint:errcheck // Test utility type assertion
	hubInstance := hub.DeepCopyObject().(ConvertibleObject)       //nolint:errcheck // Test utility type assertion

	// Convert: Original -> Hub
	convertible, ok := originalCopy.(Convertible)
	if !ok {
		t.Fatalf("Original type %T does not implement Convertible", originalCopy)
	}

	hubConvertible, ok := hubInstance.(conversion.Hub)
	if !ok {
		t.Fatalf("Hub type %T does not implement conversion.Hub", hubInstance)
	}

	if err := convertible.ConvertTo(hubConvertible); err != nil {
		t.Fatalf("Failed to convert original to hub: %v", err)
	}

	// Convert: Hub -> Original
	finalCopy := original.DeepCopyObject().(ConvertibleObject) //nolint:errcheck // Test utility type assertion
	finalConvertible, ok := finalCopy.(Convertible)
	if !ok {
		t.Fatalf("Final type %T does not implement Convertible", finalCopy)
	}

	if err := finalConvertible.ConvertFrom(hubConvertible); err != nil {
		t.Fatalf("Failed to convert hub back to original: %v", err)
	}

	// Compare original with final result
	if diff := compareObjects(original, finalCopy); diff != "" {
		t.Errorf("Round-trip conversion mismatch (-original +final):\n%s", diff)
	}
}

func testHubToOriginalToHub(t *testing.T, hub, original ConvertibleObject) {
	t.Helper()

	// Create deep copies to avoid mutation
	hubCopy := hub.DeepCopyObject().(ConvertibleObject)               //nolint:errcheck // Test utility type assertion
	originalInstance := original.DeepCopyObject().(ConvertibleObject) //nolint:errcheck // Test utility type assertion

	// Convert: Hub -> Original
	originalConvertible, ok := originalInstance.(Convertible)
	if !ok {
		t.Fatalf("Original type %T does not implement Convertible", originalInstance)
	}

	hubConvertible, ok := hubCopy.(conversion.Hub)
	if !ok {
		t.Fatalf("Hub type %T does not implement conversion.Hub", hubCopy)
	}

	if err := originalConvertible.ConvertFrom(hubConvertible); err != nil {
		t.Fatalf("Failed to convert hub to original: %v", err)
	}

	// Convert: Original -> Hub
	finalCopy := hub.DeepCopyObject().(ConvertibleObject) //nolint:errcheck // Test utility type assertion
	finalHubConvertible, ok := finalCopy.(conversion.Hub)
	if !ok {
		t.Fatalf("Final hub type %T does not implement conversion.Hub", finalCopy)
	}

	if err := originalConvertible.ConvertTo(finalHubConvertible); err != nil {
		t.Fatalf("Failed to convert original back to hub: %v", err)
	}

	// Compare hub with final result
	if diff := compareObjects(hub, finalCopy); diff != "" {
		t.Errorf("Round-trip conversion mismatch (-hub +final):\n%s", diff)
	}
}

// compareObjects compares two objects while ignoring status and handling semantic equality
func compareObjects(obj1, obj2 ConvertibleObject) string {
	return cmp.Diff(obj1, obj2, getCmpOptions()...)
}

// getCmpOptions returns comparison options that normalize differences and ignore status
func getCmpOptions() []cmp.Option {
	return []cmp.Option{
		// Ignore TypeMeta differences (apiVersion/kind changes are expected)
		cmpopts.IgnoreFields(metav1.TypeMeta{}, "APIVersion", "Kind"),

		// Ignore status fields - conversions only touch spec

		// Ignore ObjectMeta fields that change during conversion
		cmpopts.IgnoreFields(metav1.ObjectMeta{}, "CreationTimestamp", "Generation", "ResourceVersion", "UID"),

		// Sort slices by name for consistent comparison
		cmpopts.SortSlices(func(a, b interface{}) bool {
			return getFieldName(a) < getFieldName(b)
		}),

		// Handle nil vs empty slice/map differences
		cmpopts.EquateEmpty(),

		// Custom comparator for common types
		cmp.Comparer(func(x, y *string) bool {
			if x == nil && y == nil {
				return true
			}
			if x == nil || y == nil {
				return false
			}
			return *x == *y
		}),
	}
}

// getFieldName extracts a name field from an object for sorting
func getFieldName(obj interface{}) string {
	if obj == nil {
		return ""
	}

	v := reflect.ValueOf(obj)
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}

	if v.Kind() != reflect.Struct {
		return ""
	}

	// Try common name fields
	nameFields := []string{"Name", "name"}
	for _, field := range nameFields {
		if f := v.FieldByName(field); f.IsValid() && f.Kind() == reflect.String {
			return f.String()
		}
	}

	return ""
}

// ExpectConversionError tests that conversion fails with expected error
func ExpectConversionError(t *testing.T, original, hub ConvertibleObject, expectedErrorSubstring string) {
	t.Helper()

	originalCopy := original.DeepCopyObject().(ConvertibleObject) //nolint:errcheck // Test utility type assertion
	hubInstance := hub.DeepCopyObject().(ConvertibleObject)       //nolint:errcheck // Test utility type assertion

	convertible, ok := originalCopy.(Convertible)
	if !ok {
		t.Fatalf("Original type %T does not implement Convertible", originalCopy)
	}

	hubConvertible, ok := hubInstance.(conversion.Hub)
	if !ok {
		t.Fatalf("Hub type %T does not implement conversion.Hub", hubInstance)
	}

	err := convertible.ConvertTo(hubConvertible)
	if err == nil {
		t.Fatal("Expected conversion to fail, but it succeeded")
	}

	if expectedErrorSubstring != "" && !contains(err.Error(), expectedErrorSubstring) {
		t.Errorf("Expected error to contain %q, but got: %v", expectedErrorSubstring, err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) &&
		(s == substr ||
			len(s) > len(substr) &&
				(s[:len(substr)] == substr ||
					s[len(s)-len(substr):] == substr ||
					containsAt(s, substr)))
}

func containsAt(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
