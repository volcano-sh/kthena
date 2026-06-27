from fastapi import APIRouter, HTTPException
from .schema import EncodeRequest, LoadRequest, UnLoadRequest
from .downloader import TokenizerManager, TokenizerDownloader
import logging

router = APIRouter()

logger = logging.getLogger(__name__)

manager = TokenizerManager()
downloader = TokenizerDownloader()


@router.post("/v1/encode")
async def encode(req: EncodeRequest):
    encoded, num_tokens = encoder(req.model_server_id,req.text )

    if req.return_tokens:
        return {
            "encoded_tokens": encoded.tokens,
            "encoded_ids": encoded.ids,
            "num_tokens": num_tokens
        }

    return {
        "encoded_ids": encoded.ids,
        "num_tokens": num_tokens
    }


@router.post("/v1/load")
async def load(req: LoadRequest):
    try:
        tokenizer = downloader.download_tokenizer_from_huggingface(req.model_uri ,req.modelrevision)

        manager.load_tokenizer(
            req.model_server_id,
            tokenizer
        )

    except Exception as e:
        logger.exception(f"Failed to load tokenizer for {req.model_server_id}")

        raise HTTPException(status_code=500,detail=f"Failed to load tokenizer: {e}")

    return {
        "message": "Tokenizer loaded successfully",
        "model_server_id": req.model_server_id,
        "loaded": True
    }


@router.post("/v1/unload")
async def unload(req: UnLoadRequest):
    manager.unload_tokenizer(req.model_server_id)

    return {
        "message": "Tokenizer unloaded successfully",
        "model_server_id": req.model_server_id
    }


@router.get("/v1/list")
async def list_tokenizers():
    return {
        "message": "List of loaded tokenizers",
        "tokenizers": manager.list_tokenizers()
    }

def encoder(model_server_id: str, text: str):
    tokenizer = manager.get_tokenizer(model_server_id)

    if tokenizer is None:
        raise HTTPException(status_code=404,detail=f"Tokenizer '{model_server_id}' not found")

    try:
        encoded = tokenizer.encode(text)

    except Exception as e:
        logger.exception(f"Encoding failed for tokenizer '{model_server_id}'")
        raise HTTPException(status_code=500,detail=f"Tokenizer encoding failed: {e}")

    num_tokens = len(encoded.ids)

    return encoded, num_tokens
