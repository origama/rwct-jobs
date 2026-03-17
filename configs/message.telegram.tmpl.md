┏━━━━━━━━━━━━━━━━━━━━━━
┃ 🚀 *{{ if .Role }}{{ md .Role }}{{ else }}{{ md .Title }}{{ end }}*
┃ 🏢 {{ md .Company }}
┃ 🏠 {{ md .Location }}
┃ 💰 {{ md .Salary }}
┗━━━━━━━━━━━━━━━━━━━━━━

{{- if .TechStack }}

🧩 *Stack:*{{- range $v := .TechStack }}{{- $tag := tgtag $v }}{{- if $tag }} #{{ $tag }}{{- end }}{{- end }}
{{- end }}

{{- if .SummaryIT }}

📝 *Sintesi:*  
{{ md .SummaryIT }}
{{- end }}

├ 📈 {{ md .Seniority }}
├ 🏷 {{ md .ContractType }}
├ 🌍 {{ md .Language }}
└ ⭐ {{ .JobPostQualityRank }} ({{ .JobPostQualityScore }}/100)

{{- if .Tags }}
🏷 {{- range $t := .Tags }}{{- $tag := tgtag $t }}{{- if $tag }} #{{ $tag }}{{- end }}{{- end }}
{{- end }}

🔗 [candidati]({{ .SourceURL }})
