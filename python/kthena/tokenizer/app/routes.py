from fastapi import APIRouter
from schema import EncodeRequest, LoadRequest , UnLoadRequest
from downloader import TokenizerManager , TokenizerDownloader 


router = APIRouter()



@router.post("/v1/encode")
async def encode(req: EncodeRequest):
    model_server_id = req.model_server_id
    text = req.text
    return_tokens = req.return_tokens

    encoded, num_tokens = encoder(model_server_id, text, return_tokens)


    if return_tokens:
        return {
            "encoded_tokens": encoded.tokens,
            "encoded_ids": encoded.ids,
            "num_tokens": num_tokens
        }
    else:
        return {
            "encoded_ids": encoded.ids,
            "num_tokens": num_tokens
        }
    

@router.post("/v1/load")
async def load(req: LoadRequest):
    model_server_id = req.model_server_id
    modeluri = req.model_uri
    modelrevision = req.modelrevision

    tokenizer = TokenizerDownloader().download_tokenizer(modeluri, modelrevision)
    TokenizerManager().load_tokenizer(model_server_id, tokenizer)

    return {
        "message": "Tokenizer loaded successfully",
        "model_server_id": model_server_id,
        "loaded": tokenizer is not None
    }


@router.post("/v1/unload")
async def unload(req: UnLoadRequest):
    model_server_id = req.model_server_id
    TokenizerManager().unload_tokenizer(model_server_id)
    return {
        "message": "Tokenizer unloaded successfully",
        "model_server_id": model_server_id
    }

@router.get("/v1/list")
async def list_tokenizers():
    tokenizers_list = TokenizerManager().list_tokenizers()
    return {
        "message": "List of loaded tokenizers",
        "tokenizers": tokenizers_list
    }



def encoder(model_server_id: str, text: str, return_tokens: bool ):
    tokenizer = TokenizerManager().get_tokenizer(model_server_id)
    if tokenizer is None:
        raise ValueError(f"Tokenizer with model_server_id '{model_server_id}' not found.")
    encoded = tokenizer.encode(text)
    num_tokens = len(encoded.tokens.ids)
    return encoded , num_tokens
    
    
    
