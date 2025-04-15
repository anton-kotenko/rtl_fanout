install:install_rtl_fanout_service install_rtl_tcp

rtl_fanout: *.go
	GOHOSTARCH=arm GOARCH=arm go build *.go

install_rtl_fanout:rtl_fanout
	cp rtl_fanout /usr/bin/rtl_fanout

install_rtl_fanout_service:rtl_fanout.service install_rtl_fanout
	cp rtl_fanout.service /etc/systemd/system/
	systemctl  daemon-reload
	systemctl enable rtl_fanout

install_rtl_tcp:rtl_tcp.service
	cp rtl_tcp.service /etc/systemd/system/
	systemctl  daemon-reload
	systemctl enable rtl_tcp


.PHONY:install_rtl_fanout install_rtl_fanout_service
