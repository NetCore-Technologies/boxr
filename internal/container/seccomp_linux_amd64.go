//go:build linux && amd64

package container

func seccompArchitectureAndDenylist() (uint32, []uint32) {
	return 0xc000003e, []uint32{
		101, 155, 165, 166, 167, 168, 169, 175, 176, 179,
		246, 248, 249, 250, 272, 298, 304, 308, 310, 311, 313, 321, 323,
	}
}
