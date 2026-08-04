package daemoninstall

import "testing"

func TestParseDoctorTime(t *testing.T) {
	hour, minute, err := ParseDoctorTime("04:05")
	if err != nil || hour != 4 || minute != 5 {
		t.Fatalf("ParseDoctorTime = %d:%d, %v", hour, minute, err)
	}
	for _, invalid := range []string{"4", "3:30", "03:5", "24:00", "03:60", "nope"} {
		if _, _, err := ParseDoctorTime(invalid); err == nil {
			t.Errorf("ParseDoctorTime(%q) unexpectedly succeeded", invalid)
		}
	}
}
