from fastapi import FastAPI
from app.routes import router as routes

app = FastAPI(title="Kthena Tokenizer Service")

app.include_router(routes,tags=["Tokenizer Service"])


