package sarama
import "testing"

func TestSafeWaitGroup(t *testing.T) {
	w := SafeWaitGroup{}
	w.Done()
	if w.hasPaniced == 0 {
		t.Error("Paniced not set")
	}
}
