# TransMux

[![Build Status](https://travis-ci.org/Jille/transmux.png)](https://travis-ci.org/Jille/transmux)

TransMux is a simple Go library that allows you to have multiple streams over one stream.

The first thing you do is call `transmux.WrapTransport` on your existing `io.ReadWriteCloser` to get yourself a `transmux.Transport`. From now on you shouldn't use that `io.ReadWriteCloser` anymore, but you can start creating Streams inside of it. All of those behave as `io.ReadWriteClosers` as well.

In order to avoid channel numbering conflicts transmux needs to distinguish the two sides. You can do this by passing role `Master` and `Slave`. If you don't, and pass `Unknown` transmux will do a simple handshake to determine one master. The only effect being master or slaves has is whether it internally uses odd or even numbered stream ids.
