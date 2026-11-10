package protocol

const ProjectRefusalCapability = "project-refusals-v1"
const ProjectGenerationHeader = "X-ASB-Project-Generation"

type ProjectRefusal struct {
	ErrorCode string `json:"error"`
}

func (e ProjectRefusal) Error() string { return e.ErrorCode }
func ProjectRefusalStatus(code string) int {
	switch code {
	case "project_disabled", "project_deleting":
		return 403
	case "project_deleted":
		return 410
	case "project_not_found":
		return 404
	}
	return 0
}
