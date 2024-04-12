package sipgo

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo/sip"
)

type DialogServer struct {
	dialogs    sync.Map // TODO replace with typed version
	contactHDR sip.ContactHeader
	c          *Client
}

func (s *DialogServer) loadDialog(id string) *DialogServerSession {
	val, ok := s.dialogs.Load(id)
	if !ok || val == nil {
		return nil
	}

	t := val.(*DialogServerSession)
	return t
}

// NewDialogServer provides handle for managing UAS dialog
// Contact hdr must be provided for responses
// Client is needed for termination dialog session
// In case handling different transports you should have multiple instances per transport
func NewDialogServer(client *Client, contactHDR sip.ContactHeader) *DialogServer {
	s := &DialogServer{
		dialogs:    sync.Map{},
		contactHDR: contactHDR,
		c:          client,
	}
	return s
}

// ReadInvite should read from your OnInvite handler for which it creates dialog context
// You need to use DialogServerSession for all further responses
// Do not forget to add ReadAck and ReadBye for confirming dialog and terminating
func (s *DialogServer) ReadInvite(req *sip.Request, tx sip.ServerTransaction) (*DialogServerSession, error) {
	cont := req.Contact()
	if cont == nil {
		return nil, ErrDialogInviteNoContact
	}

	ctx, cancel := context.WithCancel(context.Background())
	dtx := &DialogServerSession{
		Dialog: Dialog{
			InviteRequest: req,
			state:         atomic.Int32{},
			stateCh:       make(chan sip.DialogState, 3),
			ctx:           ctx,
			cancel:        cancel,
		},
		inviteTx: tx,
		s:        s,
	}

	return dtx, nil
}

// ReadAck should read from your OnAck handler
func (s *DialogServer) ReadAck(req *sip.Request, tx sip.ServerTransaction) error {
	id, err := sip.MakeDialogIDFromRequest(req)
	if err != nil {
		return errors.Join(ErrDialogOutsideDialog, err)
	}

	dt := s.loadDialog(id)
	if dt == nil {
		// res := sip.NewResponseFromRequest(req, sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil)
		// if err := tx.Respond(res); err != nil {
		// 	return err
		// }
		return ErrDialogDoesNotExists
	}

	dt.setState(sip.DialogStateConfirmed)

	// Acks are normally just absorbed, but in case of proxy
	// they still need to be passed
	return nil
}

// ReadAck should read from your OnBye handler
func (s *DialogServer) ReadBye(req *sip.Request, tx sip.ServerTransaction) error {
	id, err := sip.MakeDialogIDFromRequest(req)
	if err != nil {
		return err
	}

	dt := s.loadDialog(id)
	if dt == nil {
		// https://datatracker.ietf.org/doc/html/rfc3261#section-15.1.2
		// If the BYE does not
		//    match an existing dialog, the UAS core SHOULD generate a 481
		//    (Call/Transaction Does Not Exist)
		// res := sip.NewResponseFromRequest(req, sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil)
		// if err := tx.Respond(res); err != nil {
		// 	return err
		// }
		return ErrDialogDoesNotExists
	}
	defer dt.Close()
	defer dt.inviteTx.Terminate() // Terminates Invite transaction

	res := sip.NewResponseFromRequest(req, 200, "OK", nil)
	if err := tx.Respond(res); err != nil {
		return err
	}

	dt.setState(sip.DialogStateEnded)

	return nil
}

type DialogServerSession struct {
	Dialog
	inviteTx sip.ServerTransaction
	s        *DialogServer
}

// Close is always good to call for cleanup or terminating dialog state
func (s *DialogServerSession) Close() error {
	s.s.dialogs.Delete(s.ID)
	// s.setState(sip.DialogStateEnded)
	// ctx, _ := context.WithTimeout(context.Background(), transaction.Timer_B)
	// return s.Bye(ctx)
	return nil
}

// Respond should be called for Invite request, you may want to call this multiple times like
// 100 Progress or 180 Ringing
// 2xx for creating dialog or other code in case failure
//
// In case Cancel request received: ErrDialogCanceled is responded
func (s *DialogServerSession) Respond(statusCode sip.StatusCode, reason string, body []byte, headers ...sip.Header) error {
	// Must copy Record-Route headers. Done by this command
	res := sip.NewResponseFromRequest(s.InviteRequest, statusCode, reason, body)

	for _, h := range headers {
		res.AppendHeader(h)
	}

	return s.WriteResponse(res)
}

// RespondSDP is just wrapper to call 200 with SDP.
// It is better to use this when answering as it provide correct headers
func (s *DialogServerSession) RespondSDP(sdp []byte) error {
	if sdp == nil {
		return fmt.Errorf("sdp not provided")
	}
	res := sip.NewSDPResponseFromRequest(s.InviteRequest, sdp)
	return s.WriteResponse(res)
}

// WriteResponse allows passing you custom response
func (s *DialogServerSession) WriteResponse(res *sip.Response) error {
	tx := s.inviteTx

	// Must add contact header
	res.AppendHeader(&s.s.contactHDR)
	s.Dialog.InviteResponse = res

	// Do we have cancel in meantime
	select {
	case req := <-tx.Cancels():
		tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))
		return ErrDialogCanceled
	case <-tx.Done():
		// There must be some error
		return tx.Err()
	default:
	}

	if !res.IsSuccess() {
		if res.IsProvisional() {
			// This will not create dialog so we will just respond
			return tx.Respond(res)
		}

		// For final response we want to set dialog ended state
		if err := tx.Respond(res); err != nil {
			return err
		}
		s.setState(sip.DialogStateEnded)
		return nil
	}

	id, err := sip.MakeDialogIDFromResponse(res)
	if err != nil {
		return err
	}

	s.Dialog.ID = id

	// We need to make dialog present as ACK can land immediately after
	s.s.dialogs.Store(id, s)
	s.setState(sip.DialogStateEstablished)

	if err := tx.Respond(res); err != nil {
		// We could also not delete this as Close will handle cleanup
		s.s.dialogs.Delete(id)
		return err
	}

	return nil
}

func (s *DialogServerSession) Bye(ctx context.Context) error {
	state := s.state.Load()
	// In case dialog terminated
	if sip.DialogState(state) == sip.DialogStateEnded {
		return nil
	}

	cli := s.s.c
	req := s.Dialog.InviteRequest
	res := s.Dialog.InviteResponse

	if !res.IsSuccess() {
		return fmt.Errorf("can not send bye on NON success response")
	}

	// This is tricky
	defer s.Close()              // Delete our dialog always
	defer s.inviteTx.Terminate() // Terminates INVITE in all cases

	// https://datatracker.ietf.org/doc/html/rfc3261#section-15
	// However, the callee's UA MUST NOT send a BYE on a confirmed dialog
	// until it has received an ACK for its 2xx response or until the server
	// transaction times out.
	for {
		state = s.state.Load()
		if sip.DialogState(state) < sip.DialogStateConfirmed {
			select {
			case <-s.inviteTx.Done():
				// Wait until we timeout
			case <-time.After(sip.T1):
				// Recheck state
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		break
	}

	bye := newByeRequestUAS(req, res)

	// Check that we have still match same dialog
	callidHDR := bye.CallID()
	newFrom := bye.From()
	newTo := bye.To()
	byeID := sip.MakeDialogID(callidHDR.Value(), newFrom.Params["tag"], newTo.Params["tag"])
	if s.ID != byeID {
		return fmt.Errorf("non matching ID %q %q", s.ID, byeID)
	}

	// Check Route Header
	if rr := bye.Route(); rr != nil {
		bye.SetDestination(rr.Address.HostPort())
	}

	tx, err := cli.TransactionRequest(ctx, bye)
	if err != nil {
		return err
	}
	defer tx.Terminate() // Terminates current transaction

	// s.setState(sip.DialogStateEnded)

	// Wait 200
	select {
	case res := <-tx.Responses():
		if res.StatusCode != 200 {
			return ErrDialogResponse{res}
		}
		s.setState(sip.DialogStateEnded)
		return nil
	case <-tx.Done():
		return tx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// newByeRequestUAS generates request for UAS within dialog
// it does not add VIA header, as this must be handled by transport layer
func newByeRequestUAS(req *sip.Request, res *sip.Response) *sip.Request {
	// We must check record route header
	// https://datatracker.ietf.org/doc/html/rfc2543#section-6.13
	cont := req.Contact()
	bye := sip.NewRequest(sip.BYE, cont.Address)

	// Reverse from and to
	from := res.From()
	to := res.To()
	callid := res.CallID()

	newFrom := &sip.FromHeader{
		DisplayName: to.DisplayName,
		Address:     to.Address,
		Params:      to.Params,
	}

	newTo := &sip.ToHeader{
		DisplayName: from.DisplayName,
		Address:     from.Address,
		Params:      from.Params,
	}

	bye.AppendHeader(newFrom)
	bye.AppendHeader(newTo)
	bye.AppendHeader(callid)

	// TODO check correct behavior strict routing vs loose routing
	// recordRoute := req.RecordRoute()
	// if recordRoute != nil {
	// 	if recordRoute.Address.UriParams.Has("lr") {
	// 		bye.AppendHeader(&sip.RouteHeader{Address: recordRoute.Address})
	// 	} else {
	// 		/* TODO
	// 		   If the route set is not empty, and its first URI does not contain the
	// 		   lr parameter, the UAC MUST place the first URI from the route set
	// 		   into the Request-URI, stripping any parameters that are not allowed
	// 		   in a Request-URI.  The UAC MUST add a Route header field containing
	// 		   the remainder of the route set values in order, including all
	// 		   parameters.  The UAC MUST then place the remote target URI into the
	// 		   Route header field as the last value.
	// 		*/
	// 	}
	// }

	// https://datatracker.ietf.org/doc/html/rfc3261#section-16.12.1.2
	hdrs := req.GetHeaders("Record-Route")
	for i := len(hdrs) - 1; i >= 0; i-- {
		recordRoute := hdrs[i]
		bye.AppendHeader(sip.NewHeader("Route", recordRoute.Value()))
	}

	return bye
}
