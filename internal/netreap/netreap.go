package netreap

const (

	// LabelKeyNomadJobID is the name of the policy label which refers to the
	// Cilium policy name
	LabelKeyCiliumPolicyName = "cilium.policy.name"

	// LabelKeyNomadJobID is the Nomad Job ID
	LabelKeyNomadJobID = "nomad.job_id"

	// LabelKeyNomadJobID is the Nomad Namespace of the job
	LabelKeyNomadNamespace = "nomad.namespace"

	// LabelKeyNomadTaskGroupID is the Nomad task group of the job allocation
	LabelKeyNomadTaskGroupID = "nomad.task_group_id"

	// LabelSourceNetreap is a label imported from Netreap
	LabelSourceNetreap = "netreap"

	// LabelSourceNomadKeyPrefix is prefix of a Netreap label
	LabelSourceNetreapKeyPrefix = LabelSourceNetreap + "."

	// LabelSourceNomad is a label imported from Nomad
	LabelSourceNomad = "nomad"

	// LabelSourceNomadKeyPrefix is prefix of a Nomad label
	LabelSourceNomadKeyPrefix = LabelSourceNomad + "."
)

var (
	LabelCiliumPolicyName = LabelKeyCiliumPolicyName + "." + LabelSourceNetreap
)

type IgnoredLabels struct {
	IgnoreMeta []string
}
