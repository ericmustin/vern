package elastic

type Workflow struct {
	Name        string            `yaml:"name"`
	Description string            `yaml:"description,omitempty"`
	Tags        []string          `yaml:"tags,omitempty"`
	Triggers    []Trigger         `yaml:"triggers"`
	Consts      map[string]string `yaml:"consts,omitempty"`
	Steps       []Step            `yaml:"steps"`
}

type Trigger struct {
	Type string      `yaml:"type"`
	With TriggerWith `yaml:"with,omitempty"`
}

type TriggerWith struct {
	Every string `yaml:"every,omitempty"`
}

type Step struct {
	Name      string                 `yaml:"name"`
	Type      string                 `yaml:"type"`
	Foreach   string                 `yaml:"foreach,omitempty"`
	Steps     []Step                 `yaml:"steps,omitempty"`
	With      map[string]interface{} `yaml:"with,omitempty"`
	OnFailure *OnFailure             `yaml:"on-failure,omitempty"`
}

type OnFailure struct {
	Continue bool `yaml:"continue,omitempty"`
}
