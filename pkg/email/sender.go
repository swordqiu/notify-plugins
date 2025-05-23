// Copyright 2019 Yunion
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package email

import (
	"context"
	"crypto/tls"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/gomail.v2"

	"yunion.io/x/log"
	"yunion.io/x/pkg/errors"

	"yunion.io/x/notify-plugins/pkg/common"
)

type SConnectInfo struct {
	Hostname string
	Hostport int
	Username string
	Password string
	Ssl      bool
}

type SEmailSender struct {
	msgChan    chan *sSendUnit
	senders    []sSender
	senderNum  int
	chanelSize int

	configCache *common.SConfigCache
}

func (self *SEmailSender) IsReady(ctx context.Context) bool {
	return self.msgChan != nil
}

func (self *SEmailSender) UpdateConfig(ctx context.Context, configs map[string]string) error {
	self.configCache.Clean()
	self.configCache.BatchSet(configs)
	return self.restartSender()
}

func ValidateConfig(ctx context.Context, configs map[string]string) (isValid bool, msg string, err error) {
	vals, ok, noKey := common.CheckMap(configs, HOSTNAME, HOSTPORT, USERNAME, PASSWORD)
	if !ok {
		err = fmt.Errorf("require %s", noKey)
		return
	}

	port, err := strconv.Atoi(vals[1])
	if err != nil {
		err = fmt.Errorf("invalid hostport %s", vals[1])
		return
	}
	conn := SConnectInfo{
		Hostname: vals[0],
		Hostport: port,
		Username: vals[2],
		Password: vals[3],
	}

	if sslg, _ := configs[GLOBALSSL]; sslg == "true" {
		conn.Ssl = true
	} else if ssl, _ := configs[SSL]; ssl == "true" {
		conn.Ssl = true
	}
	err = validateConfig(conn)
	if err == nil {
		isValid = true
		return
	}

	switch {
	case strings.Contains(err.Error(), "535 Error"):
		msg = "Authentication failed"
	case strings.Contains(err.Error(), "timeout"):
		msg = "Connect timeout"
	case strings.Contains(err.Error(), "no such host"):
		msg = "No such host"
	default:
		msg = err.Error()
	}
	err = nil
	return
}

func (self *SEmailSender) FetchContact(ctx context.Context, related string) (string, error) {
	return "", nil
}

func (self *SEmailSender) Send(ctx context.Context, params *common.SendParam) error {
	log.Debugf("reviced msg for %s: %s", params.Contact, params.Message)
	return self.send(params)
}

func (self *SEmailSender) BatchSend(ctx context.Context, params *common.BatchSendParam) ([]*common.FailedRecord, error) {
	return common.BatchSend(ctx, params, self.Send)
}

func NewSender(config common.IServiceOptions) common.ISender {
	part := config.GetOthers().(SEmailConfigPart)
	return &SEmailSender{
		senders:     make([]sSender, config.GetSenderNum()),
		senderNum:   config.GetSenderNum(),
		chanelSize:  part.ChannelSize,
		configCache: common.NewConfigCache(),
	}
}

func (self *SEmailSender) send(args *common.SendParam) error {
	gmsg := gomail.NewMessage()
	sendAddress, _ := self.configCache.Get(SENDERADDRESS)
	if sendAddress == "" {
		sendAddress, _ = self.configCache.Get(USERNAME)
	}
	gmsg.SetHeader("From", sendAddress)
	gmsg.SetHeader("To", args.Contact)
	gmsg.SetHeader("Subject", args.Topic)
	gmsg.SetHeader("Subject", args.Title)
	gmsg.SetBody("text/html", args.Message)
	ret := make(chan bool, 1)
	self.msgChan <- &sSendUnit{gmsg, ret}
	timer := time.NewTimer(1 * time.Minute)
	defer timer.Stop()
	select {
	case suc := <-ret:
		if !suc {
			return errors.Error("send error")
		}
	case <-timer.C:
		return errors.Error("send error, time out")
	}
	return nil
}

func (self *SEmailSender) restartSender() error {
	for _, sender := range self.senders {
		sender.stop()
	}
	return self.initSender()
}

func validateConfig(connInfo SConnectInfo) error {
	errChan := make(chan error, 1)
	go func() {
		dialer := gomail.NewDialer(connInfo.Hostname, connInfo.Hostport, connInfo.Username, connInfo.Password)
		if connInfo.Ssl {
			dialer.SSL = true
		} else {
			dialer.SSL = false
			// StartLSConfig
			dialer.TLSConfig = &tls.Config{
				InsecureSkipVerify: true,
			}
		}
		sender, err := dialer.Dial()
		if err != nil {
			errChan <- err
			return
		}
		sender.Close()
		errChan <- nil
	}()

	ticker := time.Tick(10 * time.Second)
	select {
	case <-ticker:
		return errors.Error("timeout")
	case err := <-errChan:
		return err
	}
}

func (self *SEmailSender) initSender() error {
	vals, ok, noKey := self.configCache.BatchGet(HOSTNAME, PASSWORD, USERNAME, HOSTPORT)
	if !ok {
		return errors.Wrap(common.ErrConfigMiss, noKey)
	}
	hostName, password, userName, hostPortStr := vals[0], vals[1], vals[2], vals[3]
	hostPort, _ := strconv.Atoi(hostPortStr)
	dialer := gomail.NewDialer(hostName, hostPort, userName, password)
	sslg, _ := self.configCache.Get(GLOBALSSL)
	ssl, _ := self.configCache.Get(SSL)
	if sslg == "true" || ssl == "true" {
		dialer.SSL = true
		log.Infof("enable ssl")
	} else {
		dialer.SSL = false
		// StartTLS process in dialer.Dial() will use TLSConfig
		dialer.TLSConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
		log.Infof("disable ssl")
	}
	// Configs are obtained successfully, it's time to init msgChan.
	if self.msgChan == nil {
		self.msgChan = make(chan *sSendUnit, self.chanelSize)
	}
	for i := 0; i < self.senderNum; i++ {
		sender := sSender{
			number: i + 1,
			dialer: dialer,
			sender: nil,
			open:   false,
			stopC:  make(chan struct{}),
			man:    self,
		}
		self.senders[i] = sender
		go sender.Run()
	}

	log.Infof("Total %d senders.", self.senderNum)
	return nil
}

type sSender struct {
	number int
	dialer *gomail.Dialer
	sender gomail.SendCloser
	open   bool
	stopC  chan struct{}
	man    *SEmailSender

	closeFailedTimes int
}

func (self *sSender) Run() {
	var err error
Loop:
	for {
		select {
		case msg, ok := <-self.man.msgChan:
			if !ok {
				break Loop
			}
			if !self.open {
				if self.sender, err = self.dialer.Dial(); err != nil {
					log.Errorf("No.%d sender connect to email serve failed because that %s.", self.number, err.Error())
					msg.result <- false
					continue Loop
				}
				self.open = true
				if err := gomail.Send(self.sender, msg.message); err != nil {
					log.Errorf("No.%d sender send email failed because that %s.", self.number, err.Error())
					self.open = false
					msg.result <- false
					continue Loop
				}
				log.Debugf("No.%d sender send email successfully.", self.number)
				msg.result <- true
			}
		case <-self.stopC:
			break Loop
		case <-time.After(30 * time.Second):
			if self.open {
				if err = self.sender.Close(); err != nil {
					log.Errorf("No.%d sender has be idle for 30 seconds and closed failed because that %s.", self.number, err.Error())
					if self.closeFailedTimes > 2 {
						log.Infof("No.%d sender has close failed 2 times so set open as false", self.number)
						self.closeFailedTimes = 0
						self.open = false
					} else {
						self.closeFailedTimes++
					}
					continue Loop
				}
				self.open = false
				log.Infof("No.%d sender has be idle for 30 seconds so that closed temporarily.", self.number)
			}
		}
	}
}

func (self *sSender) stop() {
	// First restart
	if self.stopC == nil {
		return
	}
	close(self.stopC)
}

type sSendUnit struct {
	message *gomail.Message
	result  chan<- bool
}
