package sandbox

import "testing"

func TestStorageMountLongestBoundaryAndEscapes(t *testing.T) {
	data := "1 0 0:1 / / rw - overlay overlay rw\n2 1 8:3 / /data rw - ext4 /dev/sda3 rw\n3 1 0:2 / /data/fast\\040disk rw - tmpfs tmpfs rw\n"
	for _, tc := range []struct{ path, device, filesystem string }{{"/data/runsc", "8:3", "ext4"}, {"/database/runsc", "0:1", "overlay"}, {"/data/fast disk/runsc", "0:2", "tmpfs"}} {
		got := storageForPath(tc.path, data)
		if got.Device != tc.device || got.Filesystem != tc.filesystem {
			t.Fatalf("%s: %+v", tc.path, got)
		}
	}
	if validDevice("../etc:1") || validDevice("8:3/../../x") || !validDevice("8:3") {
		t.Fatal("invalid device path validation")
	}
}
