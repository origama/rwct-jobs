{{md .SourceLabel}}

{{if .OriginalLinks}}{{index .OriginalLinks 0}}{{else}}{{.SourceURL}}{{end}}

*Categoria:* {{md .JobCategory}}
*{{md .Role}}* @ *{{md .Company}}*
{{md .SummaryIT}}

- Seniority: {{md .Seniority}}
- Location: {{md .Location}} ({{md .RemoteType}})
- Contratto: {{md .ContractType}}
- Salary: {{md .Salary}}
- Stack: {{range $i, $v := .TechStack}}{{if $i}}, {{end}}{{md $v}}{{end}}
{{if .Tags}}- Tags: {{range $i, $t := .Tags}}{{if $i}} {{end}}#{{md $t}}{{end}}
{{end}}

{{if .OriginalLinks}}
*Link Originali*
{{range .OriginalLinks}}- {{.}}
{{end}}{{end}}

{{if .OriginalImages}}
*Immagini*
{{range .OriginalImages}}- {{.}}
{{end}}{{end}}

[Annuncio originale]({{.SourceURL}})
