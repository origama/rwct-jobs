💼 *{{md .Title}}*
🏢 *{{md .Company}}* · {{md .ContractType}}
🌍 {{md .Location}} · {{md .RemoteType}}
💰 {{md .Salary}}

📝 {{md .SummaryIT}}

🔧 *Tech:* {{if .TechStack}}{{range $i, $v := .TechStack}}{{if $i}}, {{end}}{{md $v}}{{end}}{{else}}N/D{{end}}
🌐 *Lingua:* {{md .Language}}
⭐ *Qualità:* {{md .JobPostQualityRank}} ({{.JobPostQualityScore}}/100)

🔗 [Candidati qui]({{.SourceURL}})
