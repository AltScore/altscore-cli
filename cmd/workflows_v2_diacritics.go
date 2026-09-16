package cmd

import (
	"sort"
	"strings"
	"unicode"
)

// Client-facing text authored in Spanish with every accent dropped is the most
// common cosmetic defect in a CLI-built workflow: the generator scripts are
// written in ASCII and nobody goes back over 70 rule labels. The Hub and the
// PDF render UTF-8 fine, so the client reads "Cedula", "Opinion", "Garantias"
// on every screen. Fixing it after publish is a script plus one PATCH per rule.
//
// The detector is deliberately narrow. It does not guess the language of a
// string; it looks for words that in Spanish are always written with a
// diacritic and that are NOT also English words, spelled without it, in a
// string that carries no non-ASCII character at all. "Opinion", "revision" and
// "decision" are excluded because an English tenant uses them legitimately.
var foldedSpanishWords = map[string]string{
	"cedula": "cédula", "direccion": "dirección", "garantia": "garantía", "garantias": "garantías",
	"validacion": "validación", "evaluacion": "evaluación", "informacion": "información",
	"ultimo": "último", "ultima": "última", "ultimos": "últimos", "ultimas": "últimas",
	"dias": "días", "credito": "crédito", "creditos": "créditos", "codigo": "código",
	"numero": "número", "telefono": "teléfono", "razon": "razón", "verificacion": "verificación",
	"extraccion": "extracción", "analisis": "análisis", "comite": "comité", "fisica": "física",
	"fisicas": "físicas", "juridica": "jurídica", "juridicas": "jurídicas",
	"calificacion": "calificación", "situacion": "situación", "descripcion": "descripción",
	"categoria": "categoría", "categorias": "categorías", "minimo": "mínimo", "minima": "mínima",
	"maximo": "máximo", "maxima": "máxima", "publico": "público", "publica": "pública",
	"tecnologico": "tecnológico", "tecnologica": "tecnológica", "justificacion": "justificación",
	"aprobacion": "aprobación", "resolucion": "resolución", "gestion": "gestión",
	"ejecucion": "ejecución", "expedicion": "expedición", "emision": "emisión",
	"recepcion": "recepción", "inscripcion": "inscripción", "antiguedad": "antigüedad",
	"identificacion": "identificación", "clasificacion": "clasificación",
	"declaracion": "declaración", "operacion": "operación", "condicion": "condición",
	"ubicacion": "ubicación", "notificacion": "notificación", "autorizacion": "autorización",
	"cotizacion": "cotización", "facturacion": "facturación", "poblacion": "población",
	"hectarea": "hectárea", "hectareas": "hectáreas", "vehiculo": "vehículo", "vehiculos": "vehículos",
	"pais": "país", "paises": "países", "titulo": "título", "periodo": "período",
	"semaforizacion": "semaforización", "aplicacion": "aplicación", "documentacion": "documentación",
	"informe": "", "solicitud": "",
}

func init() {
	// "solicitud" and "informe" carry no accent; they are listed above only to
	// document that common credit words WITHOUT a diacritic must not be added
	// by mistake, and are removed here so they never match.
	for w, accented := range foldedSpanishWords {
		if accented == "" {
			delete(foldedSpanishWords, w)
		}
	}
}

// diacriticsMissing reports the folded Spanish words found in s, or nil when s
// is not ASCII-only (the author does use accents there) or has none.
func diacriticsMissing(s string) []string {
	for _, r := range s {
		if r > unicode.MaxASCII {
			return nil
		}
	}
	var hits []string
	seen := map[string]bool{}
	for _, tok := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r)
	}) {
		if accented, ok := foldedSpanishWords[tok]; ok && !seen[tok] {
			seen[tok] = true
			hits = append(hits, accented)
		}
	}
	return hits
}

// adviseDiacritics aggregates the check over every human-facing string it is
// handed. Empty strings are ignored; Total counts the rest.
func adviseDiacritics(texts []string) (readabilityFinding, bool) {
	var flagged []string
	total := 0
	for _, t := range texts {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		total++
		if hits := diacriticsMissing(t); len(hits) > 0 {
			flagged = append(flagged, shorten(t, 48)+" -> "+strings.Join(hits, ", "))
		}
	}
	if len(flagged) == 0 {
		return readabilityFinding{}, false
	}
	sort.Strings(flagged)
	return readabilityFinding{
		Practice: "diacritics",
		Count:    len(flagged),
		Total:    total,
		Sample:   flagged,
		Advice: "client-facing text has Spanish words written without their accents. The Hub and " +
			"the PDF render UTF-8; write Cédula, Dirección, Garantías in labels, rule descriptions, " +
			"PDF titles and HTML headings, in the client's language",
	}, true
}

func shorten(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// humanStringsFromSpec collects what a client will read out of an apply spec:
// workflow label and description, node labels, variable and input titles, PDF
// titles and section titles, htmlBlock bodies.
func humanStringsFromSpec(spec *composeSpec) []string {
	if spec == nil {
		return nil
	}
	out := []string{spec.Label}
	if spec.Description != nil {
		out = append(out, *spec.Description)
	}
	out = append(out, titlesOf(spec.CustomVariables)...)
	out = append(out, titlesOf(spec.InputVariables)...)
	for _, n := range spec.Nodes {
		out = append(out, humanStringsFromNode(n)...)
	}
	return out
}

// humanStringsFromWorkflow is the saved-workflow counterpart used by lint. End
// task bodies come separately (GET /v2/workflows/{id} does not embed them) and
// rules are the tenant's evaluation-rules for the alias.
func humanStringsFromWorkflow(wf map[string]any, endTasks []map[string]any, rules []any) []string {
	var out []string
	if s, _ := wf["label"].(string); s != "" {
		out = append(out, s)
	}
	if s, _ := wf["description"].(string); s != "" {
		out = append(out, s)
	}
	out = append(out, titlesOf(asMap(wf["customVariables"]))...)
	out = append(out, titlesOf(asMap(wf["inputVariables"]))...)
	for _, n := range asSlice(wf["nodes"]) {
		out = append(out, humanStringsFromNode(asMap(n))...)
	}
	for _, t := range endTasks {
		out = append(out, pdfStrings(asMap(asMap(t["endConfig"])["pdfConfig"]))...)
	}
	for _, r := range rules {
		rm := asMap(r)
		for _, k := range []string{"label", "description", "alertMessage"} {
			if s, _ := rm[k].(string); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func humanStringsFromNode(n map[string]any) []string {
	var out []string
	if s, _ := n["label"].(string); s != "" {
		out = append(out, s)
	}
	if s, _ := n["description"].(string); s != "" {
		out = append(out, s)
	}
	out = append(out, titlesOf(asMap(n["inputSchema"]))...)
	out = append(out, pdfStrings(asMap(asMap(n["endConfig"])["pdfConfig"]))...)
	out = append(out, pdfStrings(asMap(asMap(asMap(n["data"])["endConfig"])["pdfConfig"]))...)
	return out
}

func titlesOf(m map[string]any) []string {
	var out []string
	for _, v := range m {
		if t, _ := asMap(v)["title"].(string); t != "" {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

func pdfStrings(pdf map[string]any) []string {
	if pdf == nil {
		return nil
	}
	var out []string
	for _, k := range []string{"title", "subtitle"} {
		if s, _ := pdf[k].(string); s != "" {
			out = append(out, s)
		}
	}
	for _, s := range asSlice(pdf["sourcesConfig"]) {
		sm := asMap(s)
		for _, k := range []string{"title", "subtitle"} {
			if v, _ := sm[k].(string); v != "" {
				out = append(out, v)
			}
		}
		for _, c := range asSlice(sm["components"]) {
			if body, _ := asMap(c)["content"].(string); body != "" {
				out = append(out, body)
			}
		}
	}
	return out
}
