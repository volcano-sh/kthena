from pydantic import BaseModel


class EncodeRequest(BaseModel):
    model_server_id: str
    text: str
    return_tokens: bool = False