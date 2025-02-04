// GoToSocial
// Copyright (C) GoToSocial Authors admin@gotosocial.org
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package streaming

import (
	"context"
	"slices"
	"time"

	"codeberg.org/gruf/go-kv"
	apiutil "github.com/superseriousbusiness/gotosocial/internal/api/util"
	"github.com/superseriousbusiness/gotosocial/internal/gtserror"
	"github.com/superseriousbusiness/gotosocial/internal/gtsmodel"
	"github.com/superseriousbusiness/gotosocial/internal/log"
	"github.com/superseriousbusiness/gotosocial/internal/oauth"
	streampkg "github.com/superseriousbusiness/gotosocial/internal/stream"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// StreamGETHandler swagger:operation GET /api/v1/streaming streamGet
//
// Initiate a websocket connection for live streaming of statuses and notifications.
//
// The scheme used should *always* be `wss`. The streaming basepath can be viewed at `/api/v1/instance`.
//
// On a successful connection, a code `101` will be returned, which indicates that the connection is being upgraded to a secure websocket connection.
//
// As long as the connection is open, various message types will be streamed into it.
//
// GoToSocial will ping the connection every 30 seconds to check whether the client is still receiving.
//
// If the ping fails, or something else goes wrong during transmission, then the connection will be dropped, and the client will be expected to start it again.
//
//	---
//	tags:
//	- streaming
//
//	produces:
//	- application/json
//
//	schemes:
//	- wss
//
//	parameters:
//	-
//		name: access_token
//		type: string
//		description: Access token for the requesting account.
//		in: query
//		required: true
//	-
//		name: stream
//		type: string
//		description: |-
//			Type of stream to request.
//
//			Options are:
//
//			`user`: receive updates for the account's home timeline.
//			`public`: receive updates for the public timeline.
//			`public:local`: receive updates for the local timeline.
//			`hashtag`: receive updates for a given hashtag.
//			`hashtag:local`: receive local updates for a given hashtag.
//			`list`: receive updates for a certain list of accounts.
//			`direct`: receive updates for direct messages.
//		in: query
//		required: true
//	-
//		name: list
//		type: string
//		description: |-
//			ID of the list to subscribe to.
//			Only used if stream type is 'list'.
//		in: query
//	-
//		name: tag
//		type: string
//		description: |-
//			Name of the tag to subscribe to.
//			Only used if stream type is 'hashtag' or 'hashtag:local'.
//		in: query
//
//	security:
//	- OAuth2 Bearer:
//		- read:streaming
//
//	responses:
//		'101':
//			schema:
//				type: object
//				properties:
//					stream:
//						type: array
//						items:
//							type: string
//							enum:
//							- user
//							- public
//							- public:local
//							- hashtag
//							- hashtag:local
//							- list
//							- direct
//					event:
//						description: |-
//							The type of event being received.
//
//							`update`: a new status has been received.
//							`notification`: a new notification has been received.
//							`delete`: a status has been deleted.
//							`filters_changed`: not implemented.
//						type: string
//						enum:
//						- update
//						- notification
//						- delete
//						- filters_changed
//					payload:
//						description: |-
//							The payload of the streamed message.
//							Different depending on the `event` type.
//
//							If present, it should be parsed as a string.
//
//							If `event` = `update`, then the payload will be a JSON string of a status.
//							If `event` = `notification`, then the payload will be a JSON string of a notification.
//							If `event` = `delete`, then the payload will be a status ID.
//						type: string
//						example: "{\"id\":\"01FC3TZ5CFG6H65GCKCJRKA669\",\"created_at\":\"2021-08-02T16:25:52Z\",\"sensitive\":false,\"spoiler_text\":\"\",\"visibility\":\"public\",\"language\":\"en\",\"uri\":\"https://gts.superseriousbusiness.org/users/dumpsterqueer/statuses/01FC3TZ5CFG6H65GCKCJRKA669\",\"url\":\"https://gts.superseriousbusiness.org/@dumpsterqueer/statuses/01FC3TZ5CFG6H65GCKCJRKA669\",\"replies_count\":0,\"reblogs_count\":0,\"favourites_count\":0,\"favourited\":false,\"reblogged\":false,\"muted\":false,\"bookmarked\":fals…//gts.superseriousbusiness.org/fileserver/01JNN207W98SGG3CBJ76R5MVDN/header/original/019036W043D8FXPJKSKCX7G965.png\",\"header_static\":\"https://gts.superseriousbusiness.org/fileserver/01JNN207W98SGG3CBJ76R5MVDN/header/small/019036W043D8FXPJKSKCX7G965.png\",\"followers_count\":33,\"following_count\":28,\"statuses_count\":126,\"last_status_at\":\"2021-08-02T16:25:52Z\",\"emojis\":[],\"fields\":[]},\"media_attachments\":[],\"mentions\":[],\"tags\":[],\"emojis\":[],\"card\":null,\"poll\":null,\"text\":\"a\"}"
//		'401':
//			description: unauthorized
//		'400':
//			description: bad request
func (m *Module) StreamGETHandler(c *gin.Context) {
	var (
		account     *gtsmodel.Account
		errWithCode gtserror.WithCode
	)

	// Try query param access token.
	token := c.Query(AccessTokenQueryKey)
	if token == "" {
		// Try fallback HTTP header provided token.
		token = c.GetHeader(AccessTokenHeader)
	}

	if token != "" {

		// Token was provided, use it to authorize stream.
		account, errWithCode = m.processor.Stream().Authorize(c.Request.Context(), token)
		if errWithCode != nil {
			apiutil.ErrorHandler(c, errWithCode, m.processor.InstanceGetV1)
			return
		}

	} else {

		// No explicit token was provided:
		// try regular oauth as a last resort.
		authed, err := oauth.Authed(c, true, true, true, true)
		if err != nil {
			errWithCode := gtserror.NewErrorUnauthorized(err, err.Error())
			apiutil.ErrorHandler(c, errWithCode, m.processor.InstanceGetV1)
			return
		}

		// Set the auth'ed account.
		account = authed.Account
	}

	// Get the initial requested stream type, if there is one.
	streamType := c.Query(StreamQueryKey)

	// By appending other query params to the streamType, we
	// can allow streaming for specific list IDs or hashtags.
	// The streamType in this case will end up looking like
	// `hashtag:example` or `list:01H3YF48G8B7KTPQFS8D2QBVG8`.
	if list := c.Query(StreamListKey); list != "" {
		streamType += ":" + list
	} else if tag := c.Query(StreamTagKey); tag != "" {
		streamType += ":" + tag
	}

	// Open a stream with the processor; this lets processor
	// functions pass messages into a channel, which we can
	// then read from and put into a websockets connection.
	stream, errWithCode := m.processor.Stream().Open(
		c.Request.Context(),
		account,
		streamType,
	)
	if errWithCode != nil {
		apiutil.ErrorHandler(c, errWithCode, m.processor.InstanceGetV1)
		return
	}

	l := log.
		WithContext(c.Request.Context()).
		WithFields(kv.Fields{
			{"username", account.Username},
			{"streamID", stream.ID},
		}...)

	// Upgrade the incoming HTTP request. This hijacks the
	// underlying connection and reuses it for the websocket
	// (non-http) protocol.
	//
	// If the upgrade fails, then Upgrade replies to the client
	// with an HTTP error response.
	wsConn, err := m.wsUpgrade.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		l.Errorf("error upgrading websocket connection: %v", err)
		close(stream.Hangup)
		return
	}

	l.Info("opened websocket connection")

	// We perform the main websocket rw loops in a separate
	// goroutine in order to let the upgrade handler return.
	// This prevents the upgrade handler from holding open any
	// throttle / rate-limit request tokens which could become
	// problematic on instances with multiple users.
	go m.handleWSConn(account.Username, wsConn, stream)
}

// handleWSConn handles a two-way websocket streaming connection.
// It will both read messages from the connection, and push messages
// into the connection. If any errors are encountered while reading
// or writing (including expected errors like clients leaving), the
// connection will be closed.
func (m *Module) handleWSConn(username string, wsConn *websocket.Conn, stream *streampkg.Stream) {
	// Create new context for the lifetime of this connection.
	ctx, cancel := context.WithCancel(context.Background())

	l := log.
		WithContext(ctx).
		WithFields(kv.Fields{
			{"username", username},
			{"streamID", stream.ID},
		}...)

	// Create ticker to send keepalive pings
	pinger := time.NewTicker(m.dTicker)

	// Read messages coming from the Websocket client connection into the server.
	go func() {
		defer cancel()
		m.readFromWSConn(ctx, username, wsConn, stream)
	}()

	// Write messages coming from the processor into the Websocket client connection.
	go func() {
		defer cancel()
		m.writeToWSConn(ctx, username, wsConn, stream, pinger)
	}()

	// Wait for either the read or write functions to close, to indicate
	// that the client has left, or something else has gone wrong.
	<-ctx.Done()

	// Tidy up underlying websocket connection.
	if err := wsConn.Close(); err != nil {
		l.Errorf("error closing websocket connection: %v", err)
	}

	// Close processor channel so the processor knows
	// not to send any more messages to this stream.
	close(stream.Hangup)

	// Stop ping ticker (tiny resource saving).
	pinger.Stop()

	l.Info("closed websocket connection")
}

// readFromWSConn reads control messages coming in from the given
// websockets connection, and modifies the subscription StreamTypes
// of the given stream accordingly after acquiring a lock on it.
//
// This is a blocking function; will return only on read error or
// if the given context is canceled.
func (m *Module) readFromWSConn(
	ctx context.Context,
	username string,
	wsConn *websocket.Conn,
	stream *streampkg.Stream,
) {
	l := log.
		WithContext(ctx).
		WithFields(kv.Fields{
			{"username", username},
			{"streamID", stream.ID},
		}...)

readLoop:
	for {
		select {
		case <-ctx.Done():
			// Connection closed.
			break readLoop

		default:
			// Read JSON objects from the client and act on them.
			var msg map[string]string
			if err := wsConn.ReadJSON(&msg); err != nil {
				// Only log an error if something weird happened.
				// See: https://www.rfc-editor.org/rfc/rfc6455.html#section-11.7
				if websocket.IsUnexpectedCloseError(err, []int{
					websocket.CloseNormalClosure,
					websocket.CloseGoingAway,
					websocket.CloseNoStatusReceived,
				}...) {
					l.Errorf("error reading from websocket: %v", err)
				}

				// The connection is gone; no
				// further streaming possible.
				break readLoop
			}

			// Messages *from* the WS connection are infrequent
			// and usually interesting, so log this at info.
			l.Infof("received message from websocket: %v", msg)

			// If the message contains 'stream' and 'type' fields, we can
			// update the set of timelines that are subscribed for events.
			updateType, ok := msg["type"]
			if !ok {
				l.Warn("'type' field not provided")
				continue
			}

			updateStream, ok := msg["stream"]
			if !ok {
				l.Warn("'stream' field not provided")
				continue
			}

			// Ignore if the updateStreamType is unknown (or missing),
			// so a bad client can't cause extra memory allocations
			if !slices.Contains(streampkg.AllStatusTimelines, updateStream) {
				l.Warnf("unknown 'stream' field: %v", msg)
				continue
			}

			updateList, ok := msg["list"]
			if ok {
				updateStream += ":" + updateList
			}

			switch updateType {
			case "subscribe":
				stream.Lock()
				stream.StreamTypes[updateStream] = true
				stream.Unlock()
			case "unsubscribe":
				stream.Lock()
				delete(stream.StreamTypes, updateStream)
				stream.Unlock()
			default:
				l.Warnf("invalid 'type' field: %v", msg)
			}
		}
	}

	l.Debug("finished reading from websocket connection")
}

// writeToWSConn receives messages coming from the processor via the
// given stream, and writes them into the given websockets connection.
// This function also handles sending ping messages into the websockets
// connection to keep it alive when no other activity occurs.
//
// This is a blocking function; will return only on write error or
// if the given context is canceled.
func (m *Module) writeToWSConn(
	ctx context.Context,
	username string,
	wsConn *websocket.Conn,
	stream *streampkg.Stream,
	pinger *time.Ticker,
) {
	l := log.
		WithContext(ctx).
		WithFields(kv.Fields{
			{"username", username},
			{"streamID", stream.ID},
		}...)

writeLoop:
	for {
		select {
		case <-ctx.Done():
			// Connection closed.
			break writeLoop

		case msg := <-stream.Messages:
			// Received a new message from the processor.
			l.Tracef("writing message to websocket: %+v", msg)
			if err := wsConn.WriteJSON(msg); err != nil {
				l.Debugf("error writing json to websocket: %v", err)
				break writeLoop
			}

			// Reset pinger on successful send, since
			// we know the connection is still there.
			pinger.Reset(m.dTicker)

		case <-pinger.C:
			// Time to send a keep-alive "ping".
			l.Trace("writing ping control message to websocket")
			if err := wsConn.WriteControl(websocket.PingMessage, nil, time.Time{}); err != nil {
				l.Debugf("error writing ping to websocket: %v", err)
				break writeLoop
			}
		}
	}

	l.Debug("finished writing to websocket connection")
}
