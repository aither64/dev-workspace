package piplan

import "github.com/aither64/dev-workspace/portal/internal/piquestions"

// VerifyNativeArguments accepts exactly the completed anchored plan text. It
// neither guesses an original input ID nor reconstructs raw JSON byte equality.
func VerifyNativeArguments(proposal Proposal, arguments []byte) error {
	if piquestions.ValidateCaller(proposal.Caller) != nil || !proposal.Complete || !proposal.NativeOrigin.Complete || proposal.NativeOrigin.EntryID != proposal.Caller.Lineage.AnchorEntryID || !piquestions.Text(proposal.Text, piquestions.MessageLimit) || Digest(proposal.Text) != proposal.TextSHA256 {
		return piquestions.ErrConflict
	}
	var native struct {
		Text string `json:"text"`
	}
	if piquestions.DecodeOriginArguments(arguments, &native) != nil || native.Text != proposal.Text {
		return piquestions.ErrConflict
	}
	return nil
}
