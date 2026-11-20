package config

// JSONSchema returns the strict JSON Schema for version 1 Boxr run configs.
// Keeping the schema in the application lets editors and CI validate config
// files without importing Boxr's Go packages.
func JSONSchema() string {
	return `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://netcore-technologies.github.io/boxr/schemas/run-config-v1.json",
  "title": "Boxr run configuration",
  "type": "object",
  "required": ["version", "options"],
  "additionalProperties": false,
  "properties": {
    "version": {"const": 1},
    "name": {"type": "string", "maxLength": 64},
    "command": {"type": "array", "items": {"type": "string", "minLength": 1}},
    "metadata": {"type": "object", "maxProperties": 64, "additionalProperties": {"type": "string"}},
    "options": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "securityProfile": {"enum": ["default", "baseline", "restricted", "untrusted"]},
        "resourceProfile": {"enum": ["default", "micro", "small", "medium", "large"]},
        "name": {"type": "string", "maxLength": 64},
        "labels": {"type": "object", "maxProperties": 64, "additionalProperties": {"type": "string"}},
        "autoRemove": {"type": "boolean"},
        "hostname": {"type": "string", "maxLength": 64},
        "memoryLimitBytes": {"type": "integer", "minimum": 0},
        "memoryHighBytes": {"type": "integer", "minimum": 0},
        "memoryLowBytes": {"type": "integer", "minimum": 0},
        "memoryMinBytes": {"type": "integer", "minimum": 0},
        "memorySwapMaxBytes": {"type": "integer", "minimum": 0},
        "cpus": {"type": "number", "minimum": 0},
        "cpuWeight": {"type": "integer", "minimum": 1, "maximum": 10000},
        "ioWeight": {"type": "integer", "minimum": 1, "maximum": 10000},
        "pidsLimit": {"type": "integer", "minimum": 0},
        "memoryOOMGroup": {"type": "boolean"},
        "workdir": {"type": "string"},
        "env": {"type": "array", "items": {"type": "string", "pattern": "^[A-Za-z_][A-Za-z0-9_]*="}},
        "rootfs": {"type": "string"},
        "overlayDir": {"type": "string"},
        "readOnlyRootfs": {"type": "boolean"},
        "mountProc": {"type": "boolean"},
        "bindMounts": {"type": "array", "items": {"type": "object", "required": ["source", "target"], "additionalProperties": false, "properties": {"source": {"type": "string"}, "target": {"type": "string"}, "readOnly": {"type": "boolean"}}}},
        "tmpfsMounts": {"type": "array", "items": {"type": "object", "required": ["target"], "additionalProperties": false, "properties": {"target": {"type": "string"}, "sizeBytes": {"type": "integer", "minimum": 0}, "mode": {"type": "integer", "minimum": 0, "maximum": 4095}}}},
        "networkMode": {"enum": ["host", "none", "bridge"]},
        "networkName": {"type": "string", "minLength": 1, "maxLength": 31, "pattern": "^[a-z0-9][a-z0-9_.-]*$"},
        "userNamespace": {"type": "boolean"},
        "noNewPrivileges": {"type": "boolean"},
        "seccompProfile": {"enum": ["", "baseline"]},
        "dropCapabilities": {"type": "array", "items": {"type": "string"}}
      }
    }
  }
}`
}
