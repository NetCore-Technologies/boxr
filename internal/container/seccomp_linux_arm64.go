//go:build linux && arm64

package container

func seccompArchitectureAndDenylist() (uint32, []uint32) {
	return 0xc00000b7, []uint32{
		39, 40, 41, 60, 97, 104, 105, 106, 117, 142,
		217, 218, 219, 224, 225, 241, 265, 268, 270, 271, 273, 280, 282,
	}
}
