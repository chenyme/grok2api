import asyncio
import os
import sys
from pathlib import Path

# Add project root to path
sys.path.append(str(Path(__file__).parent.parent))

from dotenv import load_dotenv

# Load env vars
load_dotenv(Path(__file__).parent.parent / ".env")

from app.core.config import config
from app.services.token import get_token_manager
from app.services.grok.voice import VoiceService

try:
    from livekit import rtc
except ImportError:
    print("Please install livekit: pip install livekit")
    sys.exit(1)


async def main():

    
    print("Testing Grok Voice Connection...")
    
    # Load config
    await config.load()
    
    # Get token
    token = None
    
    # 1. Try TokenManager
    try:
        token_mgr = await get_token_manager()
        token = token_mgr.get_token('ssoBasic')
        if token:
             print(f"Using Token from Manager: {token[:10]}...")
    except Exception as e:
        print(f"TokenManager failed: {e}")

    # 2. Try Env Vars
    if not token:
        token = os.getenv("SSO_TOKEN") or os.getenv("GROK_TOKEN")
        if token:
            print(f"Using Token from Env: {token[:10]}...")
    
    if not token:
        print("Error: No SSO token available. Please set SSO_TOKEN env var or check token pool.")
        return
    
    sso_token = token

    # Get Voice Token
    service = VoiceService()
    try:
        data = await service.get_token(sso_token)
        token = data.get("token")
        url = "wss://livekit.grok.com"
        
        print(f"Got LiveKit Token: {token[:20]}...")
        
    except Exception as e:
        print(f"Failed to get voice token: {e}")
        return

    # Connect to LiveKit
    print(f"Connecting to {url}...")
    room = rtc.Room()
    
    @room.on("participant_connected")
    def on_participant_connected(participant: rtc.RemoteParticipant):
        print(f"Participant connected: {participant.identity}")

    @room.on("participant_disconnected")
    def on_participant_disconnected(participant: rtc.RemoteParticipant):
        print(f"Participant disconnected: {participant.identity}")

    @room.on("track_subscribed")
    def on_track_subscribed(track: rtc.Track, publication: rtc.RemoteTrackPublication, participant: rtc.RemoteParticipant):
        print(f"Track subscribed: {publication.kind} from {participant.identity}")
        if publication.kind == rtc.TrackKind.KIND_AUDIO:
            print("Audio track received! Voice mode is working.")

    try:
        await room.connect(url, token)
        print("Connected to Room!")
        
        # Keep connection open for a few seconds
        await asyncio.sleep(10)
        
    except Exception as e:
        print(f"Failed to connect to LiveKit: {e}")
    finally:
        await room.disconnect()
        print("Disconnected")


if __name__ == "__main__":
    asyncio.run(main())
