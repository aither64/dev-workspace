package piquestions

func responseView(a ResponseRecord) ResponseView {
	status := "held"
	if a.Delivery.Phase == "pending" {
		status = "pending"
	}
	if a.Delivery.Phase == "Accepted" {
		status = "delivered"
	}
	return ResponseView{a.Input, a.Sequence, status, a.Delivery.Phase}
}
func project(q QuestionRecord) QuestionView {
	v := QuestionView{ID: q.ID, Revision: q.Revision, Recipient: q.Caller.Handle.Conversation.Member, Questions: q.Questions, Status: "pending", CanRespond: !q.Closed}
	v.Answers = map[string]Answer{}
	for _, response := range q.Responses {
		for _, answer := range response.Input.Answers {
			v.Answers[answer.QuestionID] = answer
		}
	}
	if len(q.Responses) > 0 {
		a := responseView(q.Responses[len(q.Responses)-1])
		v.LatestResponse = &a
		v.Status = a.Status
	}
	if q.Closed {
		v.Status = "closed"
	}
	return v
}
