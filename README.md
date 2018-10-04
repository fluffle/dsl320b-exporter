## D-Link DSL 320-B Prometheus Exporter

Point this at your modem's (internal, firewalled) IP address and enable
the TELNET interface. It will log in and generate Prometheus metrics
when scraped by running various diagnostic commands.

Basic usage: 

    ./dsl320b-exporter --modem_ip 192.168.1.1 --modem_pass PASSWORD

### Limitations

So many. This is not polished code, by any stretch of the imagination.

The biggest one is that as yet there's no automatic reconnection of
the telnet session, so if the exporter gets disconnected for any reason
it will return bad data when scraped.

You may find your diagnostic output differs from mine, too!

### Metrics

See collectors.go. I'll probably write a list at some point when it
changes less frequently.
