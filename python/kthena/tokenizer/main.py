from fastapi import FastAPI
from app import routers

app = FastAPI()

for router in routers:
    app.include_router(router)