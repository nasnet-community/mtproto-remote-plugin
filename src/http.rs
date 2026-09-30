//! A minimal HTTP/1.1 client for one request over any byte stream — here, an
//! SSH direct-tcpip channel to the telemt API on the server's loopback.

use std::io;

use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};

/// Largest response the client reads.
const MAX_RESPONSE: usize = 8 * 1024 * 1024;

#[derive(Debug, Clone, PartialEq)]
pub struct Response {
    pub status: u16,
    pub body: Vec<u8>,
}

fn invalid(message: impl Into<String>) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message.into())
}

/// Sends one request with `Connection: close` and reads the whole response.
pub async fn request<S>(
    mut stream: S,
    method: &str,
    path: &str,
    authorization: Option<&str>,
    json_body: Option<&[u8]>,
) -> io::Result<Response>
where
    S: AsyncRead + AsyncWrite + Unpin,
{
    let mut head = format!(
        "{method} {path} HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\nAccept: application/json\r\n"
    );
    if let Some(auth) = authorization {
        head.push_str(&format!("Authorization: {auth}\r\n"));
    }
    let body = json_body.unwrap_or_default();
    if json_body.is_some() {
        head.push_str(&format!(
            "Content-Type: application/json\r\nContent-Length: {}\r\n",
            body.len()
        ));
    }
    head.push_str("\r\n");
    stream.write_all(head.as_bytes()).await?;
    stream.write_all(body).await?;
    stream.flush().await?;

    let mut raw = Vec::new();
    (&mut stream)
        .take(MAX_RESPONSE as u64 + 1)
        .read_to_end(&mut raw)
        .await?;
    if raw.len() > MAX_RESPONSE {
        return Err(invalid("response too large"));
    }
    parse(&raw)
}

pub fn parse(raw: &[u8]) -> io::Result<Response> {
    let end = raw
        .windows(4)
        .position(|w| w == b"\r\n\r\n")
        .ok_or_else(|| invalid("incomplete response head"))?;
    let head = std::str::from_utf8(&raw[..end]).map_err(|_| invalid("non-UTF-8 head"))?;
    let rest = &raw[end + 4..];

    let mut lines = head.split("\r\n");
    let status = lines
        .next()
        .and_then(|l| l.split(' ').nth(1))
        .and_then(|s| s.parse::<u16>().ok())
        .ok_or_else(|| invalid("bad status line"))?;

    let (mut length, mut chunked) = (None, false);
    for line in lines {
        let Some((name, value)) = line.split_once(':') else {
            continue;
        };
        let value = value.trim();
        if name.eq_ignore_ascii_case("content-length") {
            length = Some(
                value
                    .parse::<usize>()
                    .map_err(|_| invalid("bad content-length"))?,
            );
        } else if name.eq_ignore_ascii_case("transfer-encoding") {
            chunked = value.to_ascii_lowercase().contains("chunked");
        }
    }

    let body = if chunked {
        dechunk(rest)?
    } else if let Some(n) = length {
        rest.get(..n)
            .ok_or_else(|| invalid("body shorter than content-length"))?
            .to_vec()
    } else {
        rest.to_vec()
    };
    Ok(Response { status, body })
}

fn dechunk(mut data: &[u8]) -> io::Result<Vec<u8>> {
    let mut out = Vec::new();
    loop {
        let eol = data
            .windows(2)
            .position(|w| w == b"\r\n")
            .ok_or_else(|| invalid("truncated chunk size"))?;
        let size_text = std::str::from_utf8(&data[..eol]).map_err(|_| invalid("bad chunk"))?;
        let size_text = size_text.split(';').next().unwrap_or_default().trim();
        let size = usize::from_str_radix(size_text, 16).map_err(|_| invalid("bad chunk size"))?;
        data = &data[eol + 2..];
        if size == 0 {
            return Ok(out);
        }
        let chunk = data.get(..size).ok_or_else(|| invalid("truncated chunk"))?;
        out.extend_from_slice(chunk);
        data = data
            .get(size + 2..)
            .ok_or_else(|| invalid("truncated chunk"))?;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_a_content_length_body() {
        let raw = b"HTTP/1.1 201 Created\r\ncontent-type: application/json\r\ncontent-length: 4\r\n\r\n{}{}extra";
        let res = parse(raw).unwrap();
        assert_eq!(res.status, 201);
        assert_eq!(res.body, b"{}{}");
    }

    #[test]
    fn parses_a_chunked_body() {
        let raw = b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n3\r\n{\"a\r\n4;ext=1\r\n\":1}\r\n0\r\n\r\n";
        assert_eq!(parse(raw).unwrap().body, b"{\"a\":1}");
    }

    #[test]
    fn reads_to_close_without_a_length() {
        let raw = b"HTTP/1.1 404 Not Found\r\n\r\nnope";
        let res = parse(raw).unwrap();
        assert_eq!((res.status, res.body.as_slice()), (404, b"nope".as_slice()));
    }

    #[test]
    fn rejects_truncated_responses() {
        assert!(parse(b"HTTP/1.1 200 OK\r\n").is_err());
        assert!(parse(b"HTTP/1.1 200 OK\r\ncontent-length: 10\r\n\r\nshort").is_err());
        assert!(parse(b"HTTP/1.1 200 OK\r\ntransfer-encoding: chunked\r\n\r\n5\r\nab").is_err());
    }

    #[tokio::test]
    async fn sends_method_path_auth_and_body() {
        let (client, mut server) = tokio::io::duplex(4096);
        let server_task = tokio::spawn(async move {
            let mut request = String::new();
            let mut buf = [0u8; 4096];
            while !request.ends_with("{\"x\":1}") {
                let n = server.read(&mut buf).await.unwrap();
                request.push_str(&String::from_utf8_lossy(&buf[..n]));
            }
            server
                .write_all(b"HTTP/1.1 200 OK\r\ncontent-length: 2\r\n\r\n{}")
                .await
                .unwrap();
            request
        });
        let res = request(
            client,
            "POST",
            "/v1/users",
            Some("Bearer t"),
            Some(b"{\"x\":1}"),
        )
        .await
        .unwrap();
        assert_eq!(res.body, b"{}");
        let sent = server_task.await.unwrap();
        assert!(sent.starts_with("POST /v1/users HTTP/1.1\r\n"), "{sent}");
        assert!(sent.contains("\r\nAuthorization: Bearer t\r\n"));
        assert!(sent.contains("\r\nContent-Length: 7\r\n"));
        assert!(sent.ends_with("\r\n\r\n{\"x\":1}"));
    }
}
