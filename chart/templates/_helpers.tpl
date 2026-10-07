{{/* Omit null/empty policies; normalize only absent file policies, never merge custom limits. */}}
{{- define "task-proxy.logging" -}}
{{- $config := omit . "rotationPolicy" -}}
{{- $policy := .rotationPolicy -}}
{{- if and $policy (ne .writerType "file") -}}
{{- fail "rotationPolicy requires writerType: file" -}}
{{- end -}}
{{- if eq .writerType "file" -}}
{{- if not $policy -}}
{{- $policy = dict "rotationPeriodMilliseconds" 3600000 "maxSegmentSize" "100Mi" "maxTotalSizeToKeep" "1Gi" "maxSegmentCountToKeep" 10 -}}
{{- end -}}
{{- $_ := set $config "rotationPolicy" $policy -}}
{{- end -}}
{{- toYaml $config -}}
{{- end -}}
