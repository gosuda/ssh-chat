# Middleware reply compatibility

`AIResponse.format` is optional. Its default, `SNAPSHOT = 0`, preserves the
original protocol: each nonempty message replaces the current reply text.
`DELTA = 1` appends text and must be selected explicitly on each delta message.
A stream may switch formats for a reply.

## Deployment order

Upgrade the server before enabling `DELTA` in a client. `StreamChat` has no
capability handshake. An older server ignores field 8 and interprets each
fragment as a complete snapshot, so enabling deltas before upgrading the
server loses earlier fragments. A client that cannot establish server support
through deployment configuration must continue sending `SNAPSHOT`.

## Empty messages

An empty nonfinal snapshot clears an existing reply in storage and in the
stream's pending state. An empty nonfinal delta leaves that reply unchanged.
Neither form creates a new reply when no pending reply exists.

An empty final message retains the legacy end marker: it finishes the pending
reply without replacing the last stored text, in either format. To clear a
reply and then finish it, send an empty nonfinal snapshot followed by the empty
final marker. A nonempty final message applies its selected format and then
finishes the reply.
