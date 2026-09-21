package agentteams

// StringPresence distinguishes omitted browser input from an explicit empty
// value while validating a direct team selection.
type StringPresence struct {
	Present bool
	Value   string
}

type CreationSelectionInput struct {
	Team            StringPresence
	CatalogDigest   StringPresence
	Model           StringPresence
	ReasoningEffort StringPresence
}
