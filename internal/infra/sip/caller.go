package sip

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/miguelangel-nubla/sendspin-voip/internal/app"
	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
)

// CallerConfig holds SIP adapter connection parameters.
type CallerConfig struct {
	Mode                   string
	Server                 string
	Username               string
	Password               string
	Domain                 string
	Transport              string
	LocalIP                string
	LocalSIPPort           int
	AutoAnswerPreset       domain.AutoAnswerPreset
	CustomAutoAnswerHeader string
}

// Caller implements app.SIPCallerPort using sipgo.
type Caller struct {
	logger        *slog.Logger
	config        CallerConfig
	ua            *sipgo.UserAgent
	client        *sipgo.Client
	server        *sipgo.Server
	dialogCache   *sipgo.DialogClientCache
	localIP       string
	fromDomain    string
	activeDialogs map[string]*DialogWrapper
	registered    bool
	lastRegister  time.Time
	mu            sync.Mutex
}

// NewCaller creates a new SIP caller adapter.
func NewCaller(logger *slog.Logger, cfg CallerConfig) (*Caller, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.LocalSIPPort <= 0 {
		cfg.LocalSIPPort = 5060
	}
	if cfg.Transport == "" {
		cfg.Transport = "udp"
	}
	if cfg.Username == "" {
		cfg.Username = "sendspin"
	}

	localIP := cmp.Or(cfg.LocalIP, detectOutboundIP(cfg.Server).String())
	fromDomain := cmp.Or(cfg.Domain, cfg.Server, localIP)
	fromDomain = strings.TrimPrefix(fromDomain, "sip:")
	if h, _, err := net.SplitHostPort(fromDomain); err == nil {
		fromDomain = h
	}

	return &Caller{
		logger:        logger,
		config:        cfg,
		localIP:       localIP,
		fromDomain:    fromDomain,
		activeDialogs: make(map[string]*DialogWrapper),
	}, nil
}

// Start starts the SIP User Agent stack.
func (c *Caller) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent("sendspin-voip/1.0"),
		sipgo.WithUserAgentHostname(c.localIP),
	)
	if err != nil {
		return fmt.Errorf("failed to create SIP UA: %w", err)
	}
	c.ua = ua

	client, err := sipgo.NewClient(ua,
		sipgo.WithClientHostname(c.localIP),
		sipgo.WithClientPort(c.config.LocalSIPPort),
	)
	if err != nil {
		return fmt.Errorf("failed to create SIP Client: %w", err)
	}
	c.client = client

	server, err := sipgo.NewServer(ua)
	if err != nil {
		return fmt.Errorf("failed to create SIP Server: %w", err)
	}
	c.server = server

	contactHDR := sip.ContactHeader{
		Address: sip.Uri{
			User: c.config.Username,
			Host: c.localIP,
			Port: c.config.LocalSIPPort,
		},
	}

	c.dialogCache = sipgo.NewDialogClientCache(client, contactHDR)

	// Handle incoming BYE requests from remote phones hanging up
	server.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		callIDHeader := req.CallID()
		var callID string
		if callIDHeader != nil {
			callID = callIDHeader.Value()
		}

		c.mu.Lock()
		dialog, exists := c.activeDialogs[callID]
		if exists {
			delete(c.activeDialogs, callID)
		}
		c.mu.Unlock()

		_ = c.dialogCache.ReadBye(req, tx)

		if dialog != nil {
			c.logger.Info("Received remote BYE from phone", "call_id", callID)
			dialog.notifyDone()
		}
	})

	server.OnCancel(func(req *sip.Request, tx sip.ServerTransaction) {
		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		_ = tx.Respond(res)
	})

	server.OnOptions(func(req *sip.Request, tx sip.ServerTransaction) {
		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		_ = tx.Respond(res)
	})

	c.logger.Info("SIP client & server starting", "local_ip", c.localIP, "local_port", c.config.LocalSIPPort)

	listenAddr := net.JoinHostPort(c.localIP, strconv.Itoa(c.config.LocalSIPPort))
	go func() {
		if err := server.ListenAndServe(ctx, strings.ToLower(c.config.Transport), listenAddr); err != nil && ctx.Err() == nil {
			c.logger.Warn("SIP server listen error", "err", err)
		}
	}()

	// In PBX mode with server & username configured, perform SIP registration and maintain lease
	if strings.EqualFold(c.config.Mode, "pbx") && c.config.Server != "" && c.config.Username != "" {
		go func() {
			regCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if err := c.register(regCtx); err != nil {
				c.logger.Warn("SIP PBX initial registration note", "err", err)
			}
		}()
		go c.registrationLoop(ctx)
	}

	return nil
}

func (c *Caller) registrationLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.logger.Debug("Refreshing SIP PBX registration...")
			regCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := c.register(regCtx); err != nil {
				c.logger.Warn("SIP PBX periodic re-registration failed", "err", err)
			}
			cancel()
		}
	}
}

func (c *Caller) register(ctx context.Context) error {
	domainHost := domain.NormalizeSIPTarget(cmp.Or(c.config.Domain, c.config.Server))

	var recipientURI sip.Uri
	if err := sip.ParseUri(domainHost, &recipientURI); err != nil {
		return fmt.Errorf("invalid PBX domain URI %q: %w", domainHost, err)
	}

	req := sip.NewRequest(sip.REGISTER, recipientURI)
	fromURI := sip.Uri{
		User: c.config.Username,
		Host: recipientURI.Host,
		Port: recipientURI.Port,
	}
	req.AppendHeader(&sip.FromHeader{Address: fromURI})
	req.AppendHeader(&sip.ToHeader{Address: fromURI})
	req.AppendHeader(&sip.ContactHeader{
		Address: sip.Uri{
			User: c.config.Username,
			Host: c.localIP,
			Port: c.config.LocalSIPPort,
		},
	})
	req.AppendHeader(sip.NewHeader("Expires", "3600"))

	res, err := c.client.Do(ctx, req, sipgo.ClientRequestRegisterBuild)
	if err != nil {
		return err
	}

	res, err = c.handleDigestAuth(ctx, c.client, req, res)
	if err != nil {
		return fmt.Errorf("SIP PBX digest auth failed: %w", err)
	}

	if res.StatusCode >= 200 && res.StatusCode < 300 {
		c.mu.Lock()
		c.registered = true
		c.lastRegister = time.Now()
		c.mu.Unlock()
		c.logger.Info("Successfully registered with SIP PBX", "server", c.config.Server, "user", c.config.Username)
		return nil
	}

	c.mu.Lock()
	c.registered = false
	c.mu.Unlock()

	return fmt.Errorf("SIP PBX registration rejected with status %d %s", res.StatusCode, res.Reason)
}

func (c *Caller) handleDigestAuth(ctx context.Context, client *sipgo.Client, req *sip.Request, res *sip.Response) (*sip.Response, error) {
	if (res.StatusCode == 401 || res.StatusCode == 407) && c.config.Password != "" {
		return client.DoDigestAuth(ctx, req, res, sipgo.DigestAuth{
			Username: c.config.Username,
			Password: c.config.Password,
		})
	}
	return res, nil
}

// RegistrationStatus returns current SIP registration and connectivity info.
func (c *Caller) RegistrationStatus() app.SIPStatus {
	c.mu.Lock()
	defer c.mu.Unlock()

	return app.SIPStatus{
		Mode:         c.config.Mode,
		Server:       c.config.Server,
		Username:     c.config.Username,
		Domain:       c.config.Domain,
		Transport:    c.config.Transport,
		LocalIP:      c.localIP,
		LocalSIPPort: c.config.LocalSIPPort,
		Registered:   c.registered,
		LastRegister: c.lastRegister,
	}
}

// Stop stops the SIP client, server, and UA.
func (c *Caller) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.server != nil {
		_ = c.server.Close()
	}
	if c.client != nil {
		_ = c.client.Close()
	}
	if c.ua != nil {
		_ = c.ua.Close()
	}
	return nil
}

// LocalIP returns the local IP address announced in SDP.
func (c *Caller) LocalIP() string {
	return c.localIP
}

// ProbeTarget queries the remote SIP endpoint via OPTIONS to determine availability and supported codecs.
func (c *Caller) ProbeTarget(ctx context.Context, targetURI string) ([]domain.Codec, error) {
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()

	if client == nil {
		return nil, fmt.Errorf("SIP caller is not started")
	}

	var recipientURI sip.Uri
	targetStr := domain.NormalizeSIPTarget(targetURI)

	if err := sip.ParseUri(targetStr, &recipientURI); err != nil {
		return nil, fmt.Errorf("invalid target URI %q: %w", targetURI, err)
	}

	fromURI := sip.Uri{
		User: c.config.Username,
		Host: c.localIP,
		Port: c.config.LocalSIPPort,
	}

	req := sip.NewRequest(sip.OPTIONS, recipientURI)
	req.AppendHeader(&sip.FromHeader{Address: fromURI})
	req.AppendHeader(&sip.ToHeader{Address: recipientURI})
	req.AppendHeader(&sip.ContactHeader{
		Address: sip.Uri{
			User: fromURI.User,
			Host: c.localIP,
			Port: c.config.LocalSIPPort,
		},
	})
	req.AppendHeader(sip.NewHeader("Accept", "application/sdp"))

	res, err := client.Do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("OPTIONS request failed: %w", err)
	}

	res, err = c.handleDigestAuth(ctx, client, req, res)
	if err != nil {
		return nil, fmt.Errorf("OPTIONS digest auth failed: %w", err)
	}

	// 486 Busy Here still proves the endpoint is reachable, so treat it as a
	// successful probe. (The `!= 200` that used to be part of this condition was
	// dead: a status cannot be both >= 400 and 200.)
	if res.StatusCode >= 400 && res.StatusCode != 486 {
		return nil, fmt.Errorf("target returned status %d %s", res.StatusCode, res.Reason)
	}

	body := string(res.Body())
	if len(body) > 0 && strings.Contains(strings.ToLower(body), "m=audio") {
		codecs := ParseSDPCodecs(body)
		if len(codecs) > 0 {
			return codecs, nil
		}
	}

	// Fallback to all standard VoIP codecs if remote replied OK without SDP body
	return domain.DefaultCodecPreferences, nil
}

// Dial initiates a SIP call with auto-answer headers and returns an active dialog.
func (c *Caller) Dial(ctx context.Context, player domain.PlayerConfig, localRTPPort int) (app.SIPDialog, error) {
	c.mu.Lock()
	dialogCache := c.dialogCache
	c.mu.Unlock()

	if dialogCache == nil {
		return nil, fmt.Errorf("SIP caller is not started")
	}

	var recipientURI sip.Uri
	targetStr := domain.NormalizeSIPTarget(player.SIPTarget)

	if err := sip.ParseUri(targetStr, &recipientURI); err != nil {
		return nil, fmt.Errorf("invalid sip target URI %q: %w", player.SIPTarget, err)
	}

	// 1. Build SDP offer
	sdpOffer, err := BuildSDPOffer(c.localIP, localRTPPort, player.Codec)
	if err != nil {
		return nil, fmt.Errorf("failed to build SDP offer: %w", err)
	}

	// 2. Build Auto-Answer headers
	preset := cmp.Or(player.AutoAnswer, c.config.AutoAnswerPreset)
	customHeader := cmp.Or(player.CustomAutoAnswerHeader, c.config.CustomAutoAnswerHeader)
	headers := c.buildAutoAnswerHeaders(preset, customHeader)
	headers = append(headers, sip.NewHeader("Content-Type", "application/sdp"))

	fromHdr := sip.FromHeader{
		DisplayName: player.Name,
		Address: sip.Uri{
			User: c.config.Username,
			Host: c.fromDomain,
		},
		Params: sip.HeaderParams{sip.HeaderKV{K: "tag", V: sip.GenerateTagN(16)}},
	}
	headers = append(headers, &fromHdr)

	c.logger.Debug("Sending SIP INVITE",
		"from", fromHdr.String(),
		"target", recipientURI.String(),
		"codec", player.Codec,
		"local_rtp_port", localRTPPort,
		"auto_answer_preset", preset,
	)

	// 3. Send INVITE dialog
	dialogSession, err := dialogCache.Invite(ctx, recipientURI, []byte(sdpOffer), headers...)
	if err != nil {
		return nil, fmt.Errorf("SIP INVITE failed: %w", err)
	}

	// Wait for answer with digest credentials if challenged
	if err := dialogSession.WaitAnswer(ctx, sipgo.AnswerOptions{
		Username: c.config.Username,
		Password: c.config.Password,
	}); err != nil {
		return nil, fmt.Errorf("SIP INVITE answer failed: %w", err)
	}

	if err := c.sendAck(ctx, dialogSession, recipientURI); err != nil {
		c.logger.Warn("Failed to send SIP ACK", "err", err)
	}

	// 4. Parse SDP answer from 200 OK
	answerBody := string(dialogSession.InviteResponse.Body())
	remoteRTP, negotiatedCodec, err := ParseRemoteSDP(answerBody, recipientURI.Host)
	if err != nil {
		_ = dialogSession.Bye(ctx)
		return nil, fmt.Errorf("failed to parse remote SDP answer: %w", err)
	}

	var callID string
	if dialogSession.InviteResponse != nil && dialogSession.InviteResponse.CallID() != nil {
		callID = dialogSession.InviteResponse.CallID().Value()
	} else if dialogSession.InviteRequest != nil && dialogSession.InviteRequest.CallID() != nil {
		callID = dialogSession.InviteRequest.CallID().Value()
	}

	dialog := &DialogWrapper{
		session:       dialogSession,
		remoteRTPAddr: remoteRTP,
		codec:         negotiatedCodec,
		callID:        callID,
		doneChan:      make(chan struct{}),
	}

	if callID != "" {
		c.mu.Lock()
		c.activeDialogs[callID] = dialog
		c.mu.Unlock()

		dialog.onBye = func() {
			c.mu.Lock()
			delete(c.activeDialogs, callID)
			c.mu.Unlock()
		}
	}

	return dialog, nil
}

func (c *Caller) buildAutoAnswerHeaders(preset domain.AutoAnswerPreset, custom string) []sip.Header {
	var headers []sip.Header

	switch preset {
	case domain.AutoAnswerIntercom:
		headers = append(headers, sip.NewHeader("Alert-Info", "Intercom"))
		headers = append(headers, sip.NewHeader("Call-Info", "<sip:sendspin-voip>;answer-after=0"))
	case domain.AutoAnswerYealink:
		headers = append(headers, sip.NewHeader("Alert-Info", "info=alert-autoanswer;delay=0"))
	case domain.AutoAnswerGrandstream:
		headers = append(headers, sip.NewHeader("Alert-Info", "Ring Answer"))
	case domain.AutoAnswerSnom:
		headers = append(headers, sip.NewHeader("Alert-Info", "<sip:sendspin-voip>;info=alert-autoanswer;delay=0"))
		headers = append(headers, sip.NewHeader("Call-Info", "<sip:sendspin-voip>;answer-after=0"))
	case domain.AutoAnswerCallInfo:
		headers = append(headers, sip.NewHeader("Call-Info", "<sip:sendspin-voip>;answer-after=0"))
	case domain.AutoAnswerPAutoAnswer:
		headers = append(headers, sip.NewHeader("P-Auto-Answer", "true"))
	case domain.AutoAnswerDefault:
		headers = append(headers, sip.NewHeader("Alert-Info", "<http://example.com>;info=alert-autoanswer"))
		headers = append(headers, sip.NewHeader("Call-Info", "<sip:sendspin-voip>;answer-after=0"))
	case domain.AutoAnswerCustom:
		if custom != "" {
			parts := strings.SplitN(custom, ":", 2)
			if len(parts) == 2 {
				headers = append(headers, sip.NewHeader(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])))
			}
		}
	case domain.AutoAnswerNone:
		// None
	}

	return headers
}

func detectOutboundIP(sipServer string) net.IP {
	// Prefer routing toward the configured SIP server (works on air-gapped LANs).
	candidates := make([]string, 0, 3)
	if sipServer != "" {
		host := sipServer
		if h, _, err := net.SplitHostPort(sipServer); err == nil {
			host = h
		}
		if host != "" {
			candidates = append(candidates, net.JoinHostPort(host, "5060"))
		}
	}
	candidates = append(candidates, "1.1.1.1:80", "8.8.8.8:80")

	for _, dest := range candidates {
		conn, err := net.DialTimeout("udp", dest, 500*time.Millisecond)
		if err != nil {
			continue
		}
		ip := conn.LocalAddr().(*net.UDPAddr).IP
		_ = conn.Close()
		if ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
			return ip
		}
	}

	// Fall back to first non-loopback IPv4 on a private interface
	ifaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				var ip net.IP
				switch v := addr.(type) {
				case *net.IPNet:
					ip = v.IP
				case *net.IPAddr:
					ip = v.IP
				}
				if ip == nil || ip.IsLoopback() {
					continue
				}
				if v4 := ip.To4(); v4 != nil {
					return v4
				}
			}
		}
	}

	return net.ParseIP("127.0.0.1")
}

// DialogWrapper wraps sipgo.DialogClientSession to implement app.SIPDialog.
type DialogWrapper struct {
	session       *sipgo.DialogClientSession
	remoteRTPAddr *net.UDPAddr
	codec         domain.Codec
	callID        string
	onBye         func()
	doneChan      chan struct{}
	once          sync.Once
}

func (d *DialogWrapper) notifyDone() {
	d.once.Do(func() {
		close(d.doneChan)
	})
	if d.onBye != nil {
		d.onBye()
	}
}

func (d *DialogWrapper) RemoteRTPAddr() *net.UDPAddr {
	return d.remoteRTPAddr
}

func (d *DialogWrapper) RemoteCodec() domain.Codec {
	return d.codec
}

// CallID returns the SIP Call-ID negotiated for this dialog.
func (d *DialogWrapper) CallID() string {
	return d.callID
}

func (d *DialogWrapper) Bye(ctx context.Context) error {
	d.notifyDone()
	if d.session != nil {
		return d.session.Bye(ctx)
	}
	return nil
}

func (d *DialogWrapper) Done() <-chan struct{} {
	return d.doneChan
}

func (c *Caller) sendAck(ctx context.Context, session *sipgo.DialogClientSession, recipientURI sip.Uri) error {
	inviteReq := session.InviteRequest
	inviteRes := session.InviteResponse
	if inviteReq == nil || inviteRes == nil {
		return fmt.Errorf("missing invite request or response for ACK")
	}

	recipient := recipientURI
	if contact := inviteRes.Contact(); contact != nil {
		recipient = contact.Address
	}

	ack := sip.NewRequest(sip.ACK, recipient)
	ack.SipVersion = inviteReq.SipVersion

	if h := inviteReq.From(); h != nil {
		ack.AppendHeader(sip.HeaderClone(h))
	}
	if h := inviteRes.To(); h != nil {
		ack.AppendHeader(sip.HeaderClone(h))
	}
	if h := inviteReq.CallID(); h != nil {
		ack.AppendHeader(sip.HeaderClone(h))
	}
	if h := inviteReq.CSeq(); h != nil {
		ack.AppendHeader(sip.HeaderClone(h))
	}
	if cseq := ack.CSeq(); cseq != nil {
		cseq.MethodName = sip.ACK
	}

	maxForwards := sip.MaxForwardsHeader(70)
	ack.AppendHeader(&maxForwards)

	// Copy Record-Route in reverse order as Route headers if present
	rrHeaders := inviteRes.GetHeaders("Record-Route")
	for i := len(rrHeaders) - 1; i >= 0; i-- {
		ack.AppendHeader(sip.NewHeader("Route", rrHeaders[i].Value()))
	}

	// In PBX mode or when dialing via a PBX server, route the ACK directly to the PBX server
	if c.config.Server != "" {
		dest := c.config.Server
		if !strings.Contains(dest, ":") {
			dest = net.JoinHostPort(dest, "5060")
		}
		ack.SetDestination(dest)
	}

	c.logger.Debug("Sending SIP ACK", "to", ack.Recipient.String(), "destination", ack.Destination())
	return session.WriteAck(ctx, ack)
}
