package cmd

import "testing"

func TestDoctorDoesNotRegisterDoltConfigCheck(t *testing.T) {
	d := newDoctorForCommand("")
	for _, check := range d.Checks() {
		if check.Name() == "dolt-config" {
			t.Fatalf("dolt-config check must not be registered; it writes runtime Dolt keys into tracked .beads/config.yaml")
		}
	}
}

func TestDoctorRegistersControlPlaneHealthCheck(t *testing.T) {
	d := newDoctorForCommand("")
	for _, check := range d.Checks() {
		if check.Name() == "control-plane-health" {
			return
		}
	}
	t.Fatal("control-plane-health check is not registered")
}
