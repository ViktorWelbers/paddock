{{/*
Container securityContext for the control plane.

Paddock's pitch is that agents run under a policy; the chart holds itself to
the same one. These are the container-level controls Pod Security Admission's
"restricted" profile requires, so the chart installs unchanged into a
namespace labelled pod-security.kubernetes.io/enforce=restricted.

readOnlyRootFilesystem goes beyond restricted and is free here: both binaries
are static, write only to /data (the SQLite volume), and get an emptyDir at
/tmp for SQLite's temp files.
*/}}
{{- define "paddock.containerSecurityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities:
  drop:
    - ALL
{{- end }}
