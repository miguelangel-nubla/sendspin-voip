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
	registered       bool
	lastRegister     time.Time
	registerInterval time.Duration
	ctx              context.Context
	cancel           context.CancelFunc
	mu               sync.Mutex
}

// NewCaller creates a new SIP caller adapter.
func NewCaller(logger *slog.Logger, cfg CallerConfig) (*Caller, error) {
	logger = cmp.Or(logger, slog.Default())
	cfg.LocalSIPPort = cmp.Or(cfg.LocalSIPPort, 5060)
	cfg.Transport = cmp.Or(cfg.Transport, "udp")
	cfg.Username = cmp.Or(cfg.Username, "sendspin")

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

	// Handle incoming in-dialog Re-INVITE requests (e.g. RFC 4028 session timer refresh or direct media renegotiation)
	server.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		callIDHeader := req.CallID()
		var callID string
		if callIDHeader != nil {
			callID = callIDHeader.Value()
		}

		c.mu.Lock()
		dialog, exists := c.activeDialogs[callID]
		c.mu.Unlock()

		if !exists || dialog == nil {
			res := sip.NewResponseFromRequest(req, 481, "Call/Transaction Does Not Exist", nil)
			_ = tx.Respond(res)
			return
		}

		select {
		case <-dialog.Done():
			res := sip.NewResponseFromRequest(req, 481, "Call/Transaction Does Not Exist", nil)
			_ = tx.Respond(res)
			return
		default:
		}

		// If Re-INVITE carries updated SDP (e.g. PBX direct media redirection or renegotiation)
		body := string(req.Body())
		if len(body) > 0 && strings.Contains(strings.ToLower(body), "m=audio") {
			host := req.Source()
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			if remoteRTP, negotiatedCodec, err := ParseRemoteSDP(body, host); err == nil {
				dialog.updateRemoteSDP(remoteRTP, negotiatedCodec)
				c.logger.Debug("Updated remote RTP from Re-INVITE SDP",
					"call_id", callID,
					"remote_rtp", remoteRTP.String(),
					"codec", negotiatedCodec,
				)
			}
		}

		// Re-INVITE session refresh: respond with 200 OK + active SDP
		dialog.mu.RLock()
		localPort := dialog.localRTPPort
		activeCodec := dialog.codec
		dialog.mu.RUnlock()

		sdpAnswer, _ := BuildSDPOffer(c.localIP, localPort, activeCodec)
		res := sip.NewResponseFromRequest(req, 200, "OK", []byte(sdpAnswer))
		res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		res.AppendHeader(sip.NewHeader("Supported", "timer"))
		res.AppendHeader(sip.NewHeader("Session-Expires", "1800;refresher=uas"))
		res.AppendHeader(&sip.ContactHeader{
			Address: sip.Uri{
				User: c.config.Username,
				Host: c.localIP,
				Port: c.config.LocalSIPPort,
			},
		})
		_ = tx.Respond(res)
		c.logger.Debug("Handled incoming SIP Re-INVITE session refresh", "call_id", callID)
	})

	server.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {
		// ACK acknowledged for in-dialog 200 OK
	})

	server.OnInfo(func(req *sip.Request, tx sip.ServerTransaction) {
		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		_ = tx.Respond(res)

		callID := ""
		if req.CallID() != nil {
			callID = req.CallID().Value()
		}

		c.mu.Lock()
		dialog := c.activeDialogs[callID]
		c.mu.Unlock()

		if dialog != nil {
			if digit := parseInfoDTMF(string(req.Body())); digit != "" {
				dialog.mu.RLock()
				handler := dialog.dtmfHandler
				dialog.mu.RUnlock()
				if handler != nil {
					handler(digit)
				}
			}
		}
	})

	server.OnNotify(func(req *sip.Request, tx sip.ServerTransaction) {
		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		_ = tx.Respond(res)
	})

	c.ctx, c.cancel = context.WithCancel(context.Background())

	c.logger.Info("SIP client & server starting", "local_ip", c.localIP, "local_port", c.config.LocalSIPPort)

	listenAddr := net.JoinHostPort(c.localIP, strconv.Itoa(c.config.LocalSIPPort))
	go func() {
		if err := server.ListenAndServe(c.ctx, strings.ToLower(c.config.Transport), listenAddr); err != nil && c.ctx.Err() == nil {
			c.logger.Warn("SIP server listen error", "err", err)
		}
	}()

	// In PBX mode with server & username configured, perform SIP registration and maintain lease
	if strings.EqualFold(c.config.Mode, "pbx") && c.config.Server != "" && c.config.Username != "" {
		go func() {
			regCtx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
			defer cancel()
			if err := c.register(regCtx); err != nil {
				c.logger.Warn("SIP PBX initial registration note", "err", err)
			}
		}()
		go c.registrationLoop(c.ctx)
	}

	return nil
}

func (c *Caller) registrationLoop(ctx context.Context) {
	interval := 50 * time.Second
	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			c.logger.Debug("Refreshing SIP PBX registration...")
			regCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.register(regCtx)
			cancel()

			if err != nil {
				c.logger.Warn("SIP PBX periodic re-registration failed, retrying soon", "err", err)
				interval = 10 * time.Second
			} else {
				c.mu.Lock()
				if c.registerInterval > 0 {
					interval = c.registerInterval
				} else {
					interval = 50 * time.Second
				}
				c.mu.Unlock()
			}
			timer.Reset(interval)
		}
	}
}

func parseExpiresHeader(res *sip.Response) time.Duration {
	if h := res.GetHeader("Expires"); h != nil {
		if sec, err := strconv.Atoi(strings.TrimSpace(h.Value())); err == nil && sec > 0 {
			return time.Duration(sec) * time.Second
		}
	}
	if contact := res.Contact(); contact != nil {
		for _, param := range contact.Params {
			if strings.EqualFold(param.K, "expires") {
				if sec, err := strconv.Atoi(strings.TrimSpace(param.V)); err == nil && sec > 0 {
					return time.Duration(sec) * time.Second
				}
			}
		}
	}
	return 3600 * time.Second
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
		grantedExpiry := parseExpiresHeader(res)
		// Proactively renew at 2/3 of lease, clamped between 20s and 50s
		renewInterval := grantedExpiry * 2 / 3
		if renewInterval > 50*time.Second {
			renewInterval = 50 * time.Second
		}
		if renewInterval < 20*time.Second {
			renewInterval = 20 * time.Second
		}

		c.mu.Lock()
		c.registered = true
		c.lastRegister = time.Now()
		c.registerInterval = renewInterval
		c.mu.Unlock()
		c.logger.Info("Successfully registered with SIP PBX",
			"server", c.config.Server,
			"user", c.config.Username,
			"lease", grantedExpiry.String(),
			"renew_interval", renewInterval.String(),
		)
		return nil
	}

	c.mu.Lock()
	c.registered = false
	c.mu.Unlock()

	return fmt.Errorf("SIP PBX registration rejected with status %d %s", res.StatusCode, res.Reason)
}

func (c *Caller) handleDigestAuth(ctx context.Context, client *sipgo.Client, req *sip.Request, res *sip.Response) (*sip.Response, error) {
	if c.config.Password == "" {
		return res, nil
	}
	// Handle 401/407 challenges with up to 2 attempts for nonce expiry / stale nonce handling
	for attempts := 0; attempts < 2 && (res.StatusCode == 401 || res.StatusCode == 407); attempts++ {
		var err error
		res, err = client.DoDigestAuth(ctx, req, res, sipgo.DigestAuth{
			Username: c.config.Username,
			Password: c.config.Password,
		})
		if err != nil {
			return res, err
		}
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

	if c.cancel != nil {
		c.cancel()
	}
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
	headers = append(headers,
		sip.NewHeader("Content-Type", "application/sdp"),
		sip.NewHeader("Supported", "timer"),
		sip.NewHeader("Session-Expires", "1800;refresher=uac"),
		sip.NewHeader("Min-SE", "90"),
	)

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
		localRTPPort:  localRTPPort,
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

var autoAnswerPresetHeaders = map[domain.AutoAnswerPreset][][2]string{
	domain.AutoAnswerIntercom: {
		{"Alert-Info", "Intercom"},
		{"Call-Info", "<sip:sendspin-voip>;answer-after=0"},
	},
	domain.AutoAnswerYealink: {
		{"Alert-Info", "info=alert-autoanswer;delay=0"},
	},
	domain.AutoAnswerGrandstream: {
		{"Alert-Info", "Ring Answer"},
	},
	domain.AutoAnswerSnom: {
		{"Alert-Info", "<sip:sendspin-voip>;info=alert-autoanswer;delay=0"},
		{"Call-Info", "<sip:sendspin-voip>;answer-after=0"},
	},
	domain.AutoAnswerCallInfo: {
		{"Call-Info", "<sip:sendspin-voip>;answer-after=0"},
	},
	domain.AutoAnswerPAutoAnswer: {
		{"P-Auto-Answer", "true"},
	},
	domain.AutoAnswerDefault: {
		{"Alert-Info", "<http://example.com>;info=alert-autoanswer"},
		{"Call-Info", "<sip:sendspin-voip>;answer-after=0"},
	},
}

func (c *Caller) buildAutoAnswerHeaders(preset domain.AutoAnswerPreset, custom string) []sip.Header {
	if preset == domain.AutoAnswerCustom {
		if custom != "" {
			if k, v, ok := strings.Cut(custom, ":"); ok {
				return []sip.Header{sip.NewHeader(strings.TrimSpace(k), strings.TrimSpace(v))}
			}
		}
		return nil
	}

	rawList, ok := autoAnswerPresetHeaders[preset]
	if !ok {
		return nil
	}
	headers := make([]sip.Header, 0, len(rawList))
	for _, h := range rawList {
		headers = append(headers, sip.NewHeader(h[0], h[1]))
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
	mu            sync.RWMutex
	remoteRTPAddr *net.UDPAddr
	codec         domain.Codec
	localRTPPort  int
	callID        string
	dtmfHandler   func(digit string)
	onSDPUpdate   func(remoteAddr *net.UDPAddr, codec domain.Codec)
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
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.remoteRTPAddr
}

func (d *DialogWrapper) RemoteCodec() domain.Codec {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.codec
}

func (d *DialogWrapper) updateRemoteSDP(addr *net.UDPAddr, codec domain.Codec) {
	d.mu.Lock()
	if addr != nil {
		d.remoteRTPAddr = addr
	}
	if codec != "" {
		d.codec = codec
	}
	fn := d.onSDPUpdate
	d.mu.Unlock()

	if fn != nil {
		fn(addr, codec)
	}
}

func (d *DialogWrapper) SetDTMFHandler(handler func(digit string)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dtmfHandler = handler
}

func (d *DialogWrapper) SetSDPUpdateHandler(handler func(remoteAddr *net.UDPAddr, codec domain.Codec)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onSDPUpdate = handler
}

func parseInfoDTMF(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "signal") {
			if _, v, ok := strings.Cut(line, "="); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
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
