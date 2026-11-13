package messaging

import "testing"

func TestNormalizePhone(t *testing.T) {
	for in, want := range map[string]string{
		"08031234567": "2348031234567", "+234 803 123 4567": "2348031234567", "2348031234567": "2348031234567",
		"8031234567": "2348031234567", "0803-123-4567": "2348031234567", "12345": "", "": "", "+44 7700 900123": "",
	} {
		if got := NormalizePhone(in); got != want {
			t.Errorf("NormalizePhone(%q) = %q, want %q", in, got, want)
		}
	}
}
