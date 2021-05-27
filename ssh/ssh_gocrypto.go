// Copyright 2013 Canonical Ltd.
// Licensed under the LGPLv3, see LICENCE file for details.

package ssh

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juju/clock"
	"github.com/juju/errors"
	"github.com/juju/mutex"
	"github.com/juju/utils/v3"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/crypto/ssh/terminal"
)

const sshDefaultPort = 22

// GoCryptoClient is an implementation of Client that
// uses the embedded go.crypto/ssh SSH client.
//
// GoCryptoClient is intentionally limited in the
// functionality that it enables, as it is currently
// intended to be used only for non-interactive command
// execution.
type GoCryptoClient struct {
	signers []ssh.Signer
}

// NewGoCryptoClient creates a new GoCryptoClient.
//
// If no signers are specified, NewGoCryptoClient will
// use the private key generated by LoadClientKeys.
func NewGoCryptoClient(signers ...ssh.Signer) (*GoCryptoClient, error) {
	return &GoCryptoClient{signers: signers}, nil
}

// Command implements Client.Command.
func (c *GoCryptoClient) Command(host string, command []string, options *Options) *Cmd {
	shellCommand := utils.CommandString(command...)
	signers := c.signers
	if len(signers) == 0 {
		signers = privateKeys()
	}
	user, host := splitUserHost(host)
	port := sshDefaultPort
	var proxyCommand []string
	var knownHostsFile string
	var strictHostKeyChecking StrictHostChecksOption
	var hostKeyAlgorithms []string
	if options != nil {
		if options.port != 0 {
			port = options.port
		}
		proxyCommand = options.proxyCommand
		knownHostsFile = options.knownHostsFile
		strictHostKeyChecking = options.strictHostKeyChecking
		hostKeyAlgorithms = options.hostKeyAlgorithms
	}
	logger.Tracef(`running (equivalent of): ssh "%s@%s" -p %d '%s'`, user, host, port, shellCommand)
	return &Cmd{impl: &goCryptoCommand{
		signers:               signers,
		user:                  user,
		addr:                  net.JoinHostPort(host, strconv.Itoa(port)),
		command:               shellCommand,
		proxyCommand:          proxyCommand,
		knownHostsFile:        knownHostsFile,
		strictHostKeyChecking: strictHostKeyChecking,
		hostKeyAlgorithms:     hostKeyAlgorithms,
	}}
}

// Copy implements Client.Copy.
//
// Copy is currently unimplemented, and will always return an error.
func (c *GoCryptoClient) Copy(args []string, options *Options) error {
	return errors.Errorf("scp command is not implemented (OpenSSH scp not available in PATH)")
}

type goCryptoCommand struct {
	signers               []ssh.Signer
	user                  string
	addr                  string
	command               string
	proxyCommand          []string
	knownHostsFile        string
	strictHostKeyChecking StrictHostChecksOption
	hostKeyAlgorithms     []string
	stdin                 io.Reader
	stdout                io.Writer
	stderr                io.Writer
	client                *ssh.Client
	sess                  *ssh.Session
}

var sshDial = ssh.Dial

var sshDialWithProxy = func(addr string, proxyCommand []string, config *ssh.ClientConfig) (*ssh.Client, error) {
	if len(proxyCommand) == 0 {
		return sshDial("tcp", addr, config)
	}
	// User has specified a proxy. Create a pipe and
	// redirect the proxy command's stdin/stdout to it.
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	for i, arg := range proxyCommand {
		arg = strings.Replace(arg, "%h", host, -1)
		if port != "" {
			arg = strings.Replace(arg, "%p", port, -1)
		}
		arg = strings.Replace(arg, "%r", config.User, -1)
		proxyCommand[i] = arg
	}
	client, server := net.Pipe()
	logger.Tracef(`executing proxy command %q`, proxyCommand)
	cmd := exec.Command(proxyCommand[0], proxyCommand[1:]...)
	cmd.Stdin = server
	cmd.Stdout = server
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	conn, chans, reqs, err := ssh.NewClientConn(client, addr, config)
	if err != nil {
		return nil, err
	}
	return ssh.NewClient(conn, chans, reqs), nil
}

func (c *goCryptoCommand) ensureSession() (*ssh.Session, error) {
	if c.sess != nil {
		return c.sess, nil
	}
	if len(c.signers) == 0 {
		return nil, errors.Errorf("no private keys available")
	}
	if c.user == "" {
		currentUser, err := user.Current()
		if err != nil {
			return nil, errors.Errorf("getting current user: %v", err)
		}
		c.user = currentUser.Username
	}
	config := &ssh.ClientConfig{
		User:              c.user,
		HostKeyCallback:   c.hostKeyCallback,
		HostKeyAlgorithms: c.hostKeyAlgorithms,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeysCallback(func() ([]ssh.Signer, error) {
				return c.signers, nil
			}),
		},
	}
	client, err := sshDialWithProxy(c.addr, c.proxyCommand, config)
	if err != nil {
		return nil, err
	}
	sess, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, err
	}
	c.client = client
	c.sess = sess
	c.sess.Stdin = WrapStdin(c.stdin)
	c.sess.Stdout = c.stdout
	c.sess.Stderr = c.stderr
	return sess, nil
}

func (c *goCryptoCommand) Start() error {
	sess, err := c.ensureSession()
	if err != nil {
		return err
	}
	if c.command == "" {
		return sess.Shell()
	}
	return sess.Start(c.command)
}

func (c *goCryptoCommand) Close() error {
	if c.sess == nil {
		return nil
	}
	err0 := c.sess.Close()
	err1 := c.client.Close()
	if err0 == nil {
		err0 = err1
	}
	c.sess = nil
	c.client = nil
	return err0
}

func (c *goCryptoCommand) Wait() error {
	if c.sess == nil {
		return errors.Errorf("command has not been started")
	}
	err := c.sess.Wait()
	c.Close()
	return err
}

func (c *goCryptoCommand) Kill() error {
	if c.sess == nil {
		return errors.Errorf("command has not been started")
	}
	return c.sess.Signal(ssh.SIGKILL)
}

func (c *goCryptoCommand) SetStdio(stdin io.Reader, stdout, stderr io.Writer) {
	c.stdin = stdin
	c.stdout = stdout
	c.stderr = stderr
}

func (c *goCryptoCommand) StdinPipe() (io.WriteCloser, io.Reader, error) {
	sess, err := c.ensureSession()
	if err != nil {
		return nil, nil, err
	}
	wc, err := sess.StdinPipe()
	return wc, sess.Stdin, err
}

func (c *goCryptoCommand) StdoutPipe() (io.ReadCloser, io.Writer, error) {
	sess, err := c.ensureSession()
	if err != nil {
		return nil, nil, err
	}
	wc, err := sess.StdoutPipe()
	return ioutil.NopCloser(wc), sess.Stdout, err
}

func (c *goCryptoCommand) StderrPipe() (io.ReadCloser, io.Writer, error) {
	sess, err := c.ensureSession()
	if err != nil {
		return nil, nil, err
	}
	wc, err := sess.StderrPipe()
	return ioutil.NopCloser(wc), sess.Stderr, err
}

func (c *goCryptoCommand) hostKeyCallback(hostname string, remote net.Addr, key ssh.PublicKey) error {
	knownHostsFile := c.knownHostsFile
	if knownHostsFile == "" {
		knownHostsFile = GoCryptoKnownHostsFile()
		if knownHostsFile == "" {
			return errors.New("known_hosts file not configured")
		}
	}

	var printError func(string) error
	term, cleanupTerm, err := getTerminal()
	if err != nil {
		return errors.Trace(err)
	} else if term != nil {
		defer cleanupTerm()
		printError = func(message string) error {
			_, err := fmt.Fprintln(term, message)
			return err
		}
	} else {
		printError = func(message string) error {
			logger.Errorf("%s", message)
			return nil
		}
	}

	matched, err := checkHostKey(hostname, remote, key, knownHostsFile, printError)
	if err != nil || matched {
		return errors.Trace(err)
	}
	// We did not find a matching key, so what we do next depends on the
	// strict host key checking configuration.

	var warnAdd bool
	switch c.strictHostKeyChecking {
	case StrictHostChecksNo:
		// Don't ask, just add.
		warnAdd = true
	case StrictHostChecksDefault, StrictHostChecksAsk:
		message := fmt.Sprintf(`The authenticity of host '%s (%s)' can't be established.
%s key fingerprint is %s.
`,
			hostname,
			remote,
			key.Type(),
			ssh.FingerprintSHA256(key),
		)
		if term == nil {
			// If we're not running in a terminal,
			// we can't ask the user if they want
			// to accept.
			logger.Errorf("%s", message)
			return errors.New("not running in a terminal, cannot prompt for verification")
		}

		// Prompt user, asking if they trust the key.
		fmt.Fprint(term, message+"Are you sure you want to continue connecting (yes/no)? ")
		for {
			line, err := term.ReadLine()
			if err != nil {
				return errors.Trace(err)
			}
			var yes bool
			switch strings.ToLower(line) {
			case "yes":
				yes = true
			case "no":
				return errors.New("Host key verification failed.")
			default:
				fmt.Fprint(term, "Please type 'yes' or 'no': ")
			}
			if yes {
				break
			}
		}
	default:
		return errors.Errorf(
			`no %s host key is known for %s and you have requested strict checking`,
			key.Type(), hostname,
		)
	}

	if knownHostsFile != os.DevNull {
		// Make sure no other process modifies the file.
		releaser, err := mutex.Acquire(mutex.Spec{
			Name:  "juju-ssh-client",
			Clock: clock.WallClock,
			Delay: time.Second,
		})
		if err != nil {
			return errors.Trace(err)
		}
		defer releaser.Release()

		// Write the file atomically, so the initial ReadAll above
		// doesn't have to hold the mutex.
		knownHostsData, err := ioutil.ReadFile(knownHostsFile)
		if err != nil && !os.IsNotExist(err) {
			return errors.Trace(err)
		}
		buf := bytes.NewBuffer(knownHostsData)
		if len(knownHostsData) > 0 && !bytes.HasSuffix(knownHostsData, []byte("\n")) {
			buf.WriteRune('\n')
		}
		buf.WriteString(knownhosts.Line([]string{hostname}, key))
		buf.WriteRune('\n')
		if err := utils.AtomicWriteFile(knownHostsFile, buf.Bytes(), 0600); err != nil {
			return errors.Trace(err)
		}
	}
	if warnAdd {
		printError(fmt.Sprintf(
			"Warning: permanently added '%s' (%s) to the list of known hosts.",
			hostname, key.Type(),
		))
	}
	return nil
}

type readLineWriter interface {
	io.Writer
	ReadLine() (string, error)
}

var getTerminal = func() (readLineWriter, func(), error) {
	if fd := int(os.Stdin.Fd()); terminal.IsTerminal(fd) {
		oldState, err := terminal.MakeRaw(fd)
		if err != nil {
			return nil, nil, errors.Trace(err)
		}
		cleanup := func() { terminal.Restore(fd, oldState) }
		return terminal.NewTerminal(os.Stdin, ""), cleanup, nil
	}
	return nil, nil, nil
}

// checkHostKey checks the given (hostname, address, public key) tuple
// against the local known-hosts database, if it exists, and returns a
// boolean indicating whether a match was found, and any errors encountered.
func checkHostKey(
	hostname string,
	remote net.Addr,
	key ssh.PublicKey,
	knownHostsFile string,
	printError func(string) error,
) (bool, error) {
	// NOTE(axw) the knownhosts code is incomplete, but enough for
	// our limited use cases. We do not support parsing a known_hosts
	// file managed by OpenSSH (due to hashed hosts, etc.), but that
	// is OK since this client exists only to support systems that
	// do not have access to OpenSSH.
	callback, err := knownhosts.New(knownHostsFile)
	if err != nil {
		if os.IsNotExist(err) {
			// The known_hosts file does not exist.
			return false, nil
		}
		return false, errors.Trace(err)
	}
	err = callback(hostname, remote, key)
	switch err := err.(type) {
	case nil:
		// Known host with matching key.
		return true, nil
	case *knownhosts.KeyError:
		if len(err.Want) == 0 {
			// Unknown host.
			return false, nil
		}
		head := fmt.Sprintf(`
@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@
@    WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!     @
@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@
IT IS POSSIBLE THAT SOMEONE IS DOING SOMETHING NASTY!
Someone could be eavesdropping on you right now (man-in-the-middle attack)!
It is also possible that a host key has just been changed.
The fingerprint for the %s key sent by the remote host is
%s.
Please contact your system administrator.
Add correct host key in %s to get rid of this message.
`[1:], key.Type(), ssh.FingerprintSHA256(key), knownHostsFile)

		var typeKey *knownhosts.KnownKey
		for i, knownKey := range err.Want {
			if knownKey.Key.Type() == key.Type() {
				typeKey = &err.Want[i]
			}
		}

		var tail string
		if typeKey != nil {
			tail = fmt.Sprintf(
				"Offending %s key in %s:%d",
				typeKey.Key.Type(),
				typeKey.Filename,
				typeKey.Line,
			)
		} else {
			tail = "Host was previously using different host key algorithms:"
			for _, knownKey := range err.Want {
				tail += fmt.Sprintf(
					"\n - %s key in %s:%d",
					knownKey.Key.Type(),
					knownKey.Filename,
					knownKey.Line,
				)
			}
		}
		if err := printError(head + tail); err != nil {
			// Not being able to display the warning
			// should be considered fatal.
			return false, errors.Annotate(
				err, "failed to print host key mismatch warning",
			)
		}
	}
	return false, errors.Trace(err)
}

func splitUserHost(s string) (user, host string) {
	userHost := strings.SplitN(s, "@", 2)
	if len(userHost) == 2 {
		return userHost[0], userHost[1]
	}
	return "", userHost[0]
}

var (
	goCryptoKnownHostsMutex sync.Mutex
	goCryptoKnownHostsFile  string
)

// GoCryptoKnownHostsFile returns the known_hosts file used
// by the golang.org/x/crypto/ssh-based client by default.
func GoCryptoKnownHostsFile() string {
	goCryptoKnownHostsMutex.Lock()
	defer goCryptoKnownHostsMutex.Unlock()
	return goCryptoKnownHostsFile
}

// SetGoCryptoKnownHostsFile returns the known_hosts file used
// by the golang.org/x/crypto/ssh-based client.
func SetGoCryptoKnownHostsFile(file string) {
	goCryptoKnownHostsMutex.Lock()
	defer goCryptoKnownHostsMutex.Unlock()
	goCryptoKnownHostsFile = file
}
