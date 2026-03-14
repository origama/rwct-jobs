┏━━━━━━━━━━━━━━━━━━━━━━
┃ 🚀 *{{ if .Role }}{{ md .Role }}{{ else }}{{ md .Title }}{{ end }}*
┃ 🏢 {{ md .Company }}
┃ 🏠 {{ md .Location }}
┃ 💰 {{ md .Salary }}
┗━━━━━━━━━━━━━━━━━━━━━━

{{- if .TechStack }}

🧩 *Stack:* {{- range $i, $v := .TechStack }}{{ if $i }}, {{ end }}#{{ md $v }}{{- end }}
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
🏷 {{- range $i, $t := .Tags }}{{ if $i }} {{ end }}#{{ md $t }}{{- end }}
{{- end }}

🔗 [candidati]({{ .SourceURL }})
