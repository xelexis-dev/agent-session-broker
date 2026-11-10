package serverruntime

import (
	"context"
	"errors"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
	"github.com/xelexis-dev/agent-session-broker/source/internal/controlpanel"
	"github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func (b panelBackend) InspectDirectRegistration(ctx context.Context, project string, r controlpanel.DirectRequest) (controlpanel.DirectProof, error) {
	service, err := b.factory.RemoteBackend(ctx, project)
	if err != nil {
		return controlpanel.DirectProof{}, err
	}
	inspector, ok := service.(interface {
		InspectDirectRegistration(context.Context, string, string, string, string, string) (broker.DirectProof, error)
	})
	if !ok {
		return controlpanel.DirectProof{}, errors.New("direct registration unsupported")
	}
	if r.Handover {
		h, ok := service.(interface {
			InspectConversationHandover(context.Context, broker.RegisterRequest, string) (broker.DirectProof, error)
		})
		if !ok {
			return controlpanel.DirectProof{}, brokerHandoverUnsupported()
		}
		p, err := h.InspectConversationHandover(ctx, broker.RegisterRequest{Holder: r.Holder, HolderProof: r.HolderProof, Harness: r.Harness, SessionID: r.SessionID, ClientID: r.InstanceID, PID: r.PID, CWD: r.CWD, RequestID: r.RequestID, Secret: r.ResumeSecret}, r.RegistrationID)
		return controlpanel.DirectProof{HandoverFrom: p.HandoverFrom, PreviousIdentity: p.PreviousIdentity, AdmissionGrantID: p.AdmissionGrantID, RegistrationID: p.RegistrationID, OwnerID: p.OwnerID, Name: p.Name, Revision: p.Revision}, err
	}
	p, err := inspector.InspectDirectRegistration(ctx, r.Harness, r.SessionID, r.InstanceID, r.RegistrationID, r.ResumeSecret)
	return controlpanel.DirectProof{Absent: p.Absent, AdmissionGrantID: p.AdmissionGrantID, RegistrationID: p.RegistrationID, OwnerID: p.OwnerID, Name: p.Name, Revision: p.Revision, Names: p.Names}, err
}

func (b panelBackend) RenameDirectRegistration(ctx context.Context, project string, r controlpanel.DirectRequest, proof controlpanel.DirectProof) (controlpanel.DirectProof, error) {
	service, err := b.factory.RemoteBackend(ctx, project)
	if err != nil {
		return controlpanel.DirectProof{}, err
	}
	renamer, ok := service.(interface {
		RenameDirectRegistration(context.Context, string, string, uint64, string) (broker.DirectProof, error)
	})
	if !ok {
		return controlpanel.DirectProof{}, errors.New("direct rename unsupported")
	}
	p, err := renamer.RenameDirectRegistration(ctx, r.RegistrationID, r.ResumeSecret, proof.Revision, r.RequestedNickname)
	return controlpanel.DirectProof{RegistrationID: p.RegistrationID, OwnerID: p.OwnerID, Name: p.Name, Revision: p.Revision, AdmissionGrantID: p.AdmissionGrantID, Names: p.Names}, err
}

func brokerHandoverUnsupported() error { return protocol.HandoverRefusal{Reason: "unsupported-mode"} }
func (b panelBackend) RebindConversation(ctx context.Context, project string, r controlpanel.DirectRequest, proof controlpanel.DirectProof) (controlpanel.DirectProof, error) {
	service, err := b.factory.RemoteBackend(ctx, project)
	if err != nil {
		return controlpanel.DirectProof{}, err
	}
	h, ok := service.(interface {
		RebindConversation(context.Context, broker.RegisterRequest, string) (broker.DirectProof, error)
	})
	if !ok {
		return controlpanel.DirectProof{}, brokerHandoverUnsupported()
	}
	p, err := h.RebindConversation(ctx, broker.RegisterRequest{Holder: r.Holder, HolderProof: r.HolderProof, Harness: r.Harness, SessionID: r.SessionID, ClientID: r.InstanceID, PID: r.PID, CWD: r.CWD, ProcessKey: r.ProcessKey, Secret: r.ResumeSecret, RequestID: r.RequestID, ExpectedRevision: proof.Revision}, r.RegistrationID)
	return controlpanel.DirectProof{HandoverFrom: p.HandoverFrom, RegistrationID: p.RegistrationID, OwnerID: p.OwnerID, Name: p.Name, Revision: p.Revision, AdmissionGrantID: p.AdmissionGrantID}, err
}

func (b panelBackend) CheckRegistrationHolder(ctx context.Context, project string, r controlpanel.DirectRequest) error {
	service, err := b.factory.RemoteBackend(ctx, project)
	if err != nil {
		return err
	}
	guard, ok := service.(interface {
		CheckRegistrationHolder(context.Context, broker.RegisterRequest, string) error
	})
	if !ok {
		return nil
	}
	return guard.CheckRegistrationHolder(ctx, broker.RegisterRequest{Harness: r.Harness, ClientID: r.InstanceID, Secret: r.ResumeSecret, Holder: r.Holder, HolderProof: r.HolderProof}, r.RegistrationID)
}

func (b panelBackend) WithDirectRegistration(ctx context.Context, project string, r controlpanel.DirectRequest, apply func(controlpanel.DirectProof) error) error {
	service, err := b.factory.RemoteBackend(ctx, project)
	if err != nil {
		return err
	}
	fenced, ok := service.(interface {
		WithDirectRegistration(context.Context, broker.RegisterRequest, string, bool, func(broker.DirectProof) error) error
	})
	if !ok {
		return errors.New("direct exchange fence unavailable")
	}
	return fenced.WithDirectRegistration(ctx, broker.RegisterRequest{Harness: r.Harness, SessionID: r.SessionID, ClientID: r.InstanceID, Secret: r.ResumeSecret, Holder: r.Holder, HolderProof: r.HolderProof, PID: r.PID, CWD: r.CWD, ProcessKey: r.ProcessKey, RequestID: r.RequestID}, r.RegistrationID, r.Handover, func(p broker.DirectProof) error {
		return apply(controlpanel.DirectProof{Absent: p.Absent, HandoverFrom: p.HandoverFrom, PreviousIdentity: p.PreviousIdentity, AdmissionGrantID: p.AdmissionGrantID, RegistrationID: p.RegistrationID, OwnerID: p.OwnerID, Name: p.Name, Revision: p.Revision, Names: p.Names})
	})
}
