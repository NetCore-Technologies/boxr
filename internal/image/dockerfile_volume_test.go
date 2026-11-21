package image

import "testing"

func TestDockerfileVolumeInstructionRecordsAndValidatesTargets(t *testing.T) {
	spec, err := ParseDockerfile([]byte("FROM scratch\nVOLUME [\"/data\", \"/cache\"]\nVOLUME /logs\n"))
	if err != nil {
		t.Fatalf("parse Dockerfile VOLUME: %v", err)
	}
	for _, target := range []string{"/data", "/cache", "/logs"} {
		if _, ok := spec.Config.Volumes[target]; !ok {
			t.Errorf("Dockerfile VOLUME target %q was not retained: %+v", target, spec.Config.Volumes)
		}
	}

	for _, dockerfile := range []string{
		"FROM scratch\nVOLUME relative\n",
		"FROM scratch\nVOLUME /\n",
		"FROM scratch\nVOLUME /data/../host\n",
		"FROM scratch\nVOLUME []\n",
	} {
		if _, err := ParseDockerfile([]byte(dockerfile)); err == nil {
			t.Errorf("unsafe or empty VOLUME instruction accepted: %q", dockerfile)
		}
	}
}

func TestBuildRuntimeConfigInheritsDeclaredVolumes(t *testing.T) {
	base := ImageRuntimeConfig{Volumes: map[string]struct{}{"/data": {}}}
	overrides := ImageRuntimeConfig{Volumes: map[string]struct{}{"/cache": {}}}
	merged := inheritBuildRuntimeConfig(base, overrides)
	if len(merged.Volumes) != 2 {
		t.Fatalf("inherited volume declarations = %#v", merged.Volumes)
	}
	if _, ok := merged.Volumes["/data"]; !ok {
		t.Fatal("base image volume declaration was lost")
	}
	if _, ok := merged.Volumes["/cache"]; !ok {
		t.Fatal("Dockerfile volume declaration was not merged")
	}
}
