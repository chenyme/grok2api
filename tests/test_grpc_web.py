"""gRPC-Web 协议工具单元测试"""

import struct

from app.services.grok.protocols.grpc_web import (
    encode_grpc_web_payload,
    parse_grpc_web_response,
    get_grpc_status,
    GrpcStatus,
)

# ==================== encode ====================


def test_encode_grpc_web_payload_format():
    data = b"hello"
    result = encode_grpc_web_payload(data)
    assert result[0:1] == b"\x00"  # data frame flag
    length = struct.unpack(">I", result[1:5])[0]
    assert length == len(data)
    assert result[5:] == data


def test_encode_empty_payload():
    result = encode_grpc_web_payload(b"")
    assert result == b"\x00\x00\x00\x00\x00"


# ==================== parse ====================


def test_parse_single_data_frame():
    payload = b"test-data"
    frame = b"\x00" + struct.pack(">I", len(payload)) + payload
    messages, trailers = parse_grpc_web_response(frame)
    assert len(messages) == 1
    assert messages[0] == payload
    assert trailers == {}


def test_parse_trailer_frame():
    trailer_text = b"grpc-status: 0\r\ngrpc-message: OK"
    frame = b"\x80" + struct.pack(">I", len(trailer_text)) + trailer_text
    messages, trailers = parse_grpc_web_response(frame)
    assert len(messages) == 0
    assert trailers["grpc-status"] == "0"
    assert trailers["grpc-message"] == "OK"


def test_parse_data_plus_trailer():
    data = b"response-body"
    trailer_text = b"grpc-status: 0"
    data_frame = b"\x00" + struct.pack(">I", len(data)) + data
    trailer_frame = b"\x80" + struct.pack(">I", len(trailer_text)) + trailer_text
    body = data_frame + trailer_frame
    messages, trailers = parse_grpc_web_response(body)
    assert len(messages) == 1
    assert messages[0] == data
    assert trailers["grpc-status"] == "0"


def test_parse_multiple_data_frames():
    frames = b""
    for i in range(3):
        payload = f"msg-{i}".encode()
        frames += b"\x00" + struct.pack(">I", len(payload)) + payload
    messages, trailers = parse_grpc_web_response(frames)
    assert len(messages) == 3
    assert messages[1] == b"msg-1"


def test_parse_compressed_frame_raises():
    payload = b"compressed"
    frame = b"\x01" + struct.pack(">I", len(payload)) + payload
    try:
        parse_grpc_web_response(frame)
        assert False, "Should raise ValueError"
    except ValueError as e:
        assert "compressed" in str(e).lower()


def test_parse_grpc_status_from_headers_fallback():
    body = b""  # 空body
    headers = {"grpc-status": "7", "grpc-message": "PERMISSION_DENIED"}
    messages, trailers = parse_grpc_web_response(body, headers=headers)
    assert trailers["grpc-status"] == "7"
    assert trailers["grpc-message"] == "PERMISSION_DENIED"


def test_parse_percent_encoded_grpc_message():
    trailer_text = b"grpc-status: 16\r\ngrpc-message: Not%20authenticated"
    frame = b"\x80" + struct.pack(">I", len(trailer_text)) + trailer_text
    _, trailers = parse_grpc_web_response(frame)
    assert trailers["grpc-message"] == "Not authenticated"


# ==================== GrpcStatus ====================


def test_grpc_status_ok():
    st = GrpcStatus(code=0, message="")
    assert st.ok
    assert st.http_equiv == 200


def test_grpc_status_unauthenticated():
    st = GrpcStatus(code=16, message="Unauthenticated")
    assert not st.ok
    assert st.http_equiv == 401


def test_grpc_status_unknown_code():
    st = GrpcStatus(code=999)
    assert st.http_equiv == 502  # 未知映射到 502


def test_get_grpc_status_from_trailers():
    trailers = {"grpc-status": "7", "grpc-message": "denied"}
    st = get_grpc_status(trailers)
    assert st.code == 7
    assert st.message == "denied"
    assert st.http_equiv == 403


def test_get_grpc_status_missing_defaults():
    st = get_grpc_status({})
    assert st.code == -1
    assert st.message == ""
