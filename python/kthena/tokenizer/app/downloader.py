
import shutil
from urllib.parse import urlparse

from tokenizers import Tokenizer
import os
import subprocess
from modelscope import snapshot_download
from tempfile import TemporaryDirectory

import logging
from typing import Tuple

logger = logging.getLogger(__name__)


_TOKENIZER_FILES = [
    "tokenizer.json",
    "tokenizer_config.json",
    "special_tokens_map.json",
    "vocab.json",
    "merges.txt",
    "vocab.txt",
]



class TokenizerManager:
    def __init__(self):
        self.tokenizers = {}

    def load_tokenizer(self, model_server_id: str, tokenizer: Tokenizer):
        self.tokenizers[model_server_id] = tokenizer

    def unload_tokenizer(self, model_server_id: str):
        if model_server_id in self.tokenizers:
            del self.tokenizers[model_server_id]

    def get_tokenizer(self, model_server_id: str) -> Tokenizer:
        return self.tokenizers.get(model_server_id)

    def list_tokenizers(self):
        return list(self.tokenizers.keys())
    


class TokenizerDownloader:
    def __init__(self):
        pass

    def download_tokenizer_from_huggingface(self, modelroute: str, revision: str | None = None) -> Tokenizer:

        if revision is not None:
            tokenizer = Tokenizer.from_pretrained(modelroute, revision=revision)
        else:
            tokenizer = Tokenizer.from_pretrained(modelroute)    

        return tokenizer
    
    def download_tokenizer_from_modelscope(self, modelroute: str, revision: str | None = None) -> Tokenizer:

        modelroute = f"{modelroute}@{revision}"
        tokenizer = DownloadHelper().modelscope_download(modelroute, revision)
        return tokenizer
    
    def download_tokenizer_from_s3(self, s3_info : str , s3_route : str) -> Tokenizer:
        with TemporaryDirectory() as output_dir:
            helper = DownloadHelper(s3_info)
            download_path = helper.downloadS3(s3_route, output_dir)
            tokenizer = Tokenizer.from_pretrained(download_path)
            return tokenizer
    
        

    def download_tokenizer_from_pvc(self, pvc_path: str) -> Tokenizer:
        with TemporaryDirectory() as output_dir:
            DownloadHelper = DownloadHelper(pvc_path).downloadPVC(output_dir)
            tokenizer = Tokenizer.from_pretrained(DownloadHelper)
            return tokenizer
    
    
  



def parse_bucket_from_model_url(url: str, scheme: str) -> Tuple[str, str]:
    result = urlparse(url, scheme=scheme)
    bucket_name = result.netloc
    bucket_path = result.path.lstrip("/")
    return bucket_name, bucket_path



class DownloadHelper:
    def __init__(self, model_uri: str, access_key: str = None, secret_key: str = None, endpoint: str = None ):
        self.access_key = access_key
        self.secret_key = secret_key
        self.endpoint = endpoint
    
    def _prepare_environment(self):
        env = os.environ.copy()
        if self.access_key:
            env['AWS_ACCESS_KEY_ID'] = self.access_key
        if self.secret_key:
            env['AWS_SECRET_ACCESS_KEY'] = self.secret_key
        if self.endpoint:
            env['AWS_ENDPOINT_URL'] = self.endpoint
        return env
    
    def downloadS3(self, source : str  , output_dir: str ,  model_uri: str = None) -> str:
        """ Sync only known tokenizer files from S3. """
        uri = model_uri or self.model_uri or source
        logger.info(f"Downloading tokenizer from: {uri}")
        os.makedirs(output_dir, exist_ok=True)
        
        bucket, prefix = parse_bucket_from_model_url(uri, "s3")
        source = f"s3://{bucket}/{prefix}"
        
        # Build sync command with include patterns
        cmd = ['aws', 's3', 'sync', source, output_dir, '--exclude', '*']
        for file in _TOKENIZER_FILES:
            cmd.extend(['--include', file])
        
        try:
            subprocess.run(cmd, env=self._prepare_environment(), check=True, capture_output=True)
            logger.info(f"✓ Downloaded to: {output_dir}")
            return output_dir
        except subprocess.CalledProcessError as e:
            logger.error(f"S3 sync failed: {e.stderr}")
            raise

    def downloadPVC(self, output_dir: str ) -> str:
        logger.info(f"Downloading tokenizer from PVC: {self.model_uri}")
        os.makedirs(output_dir, exist_ok=True)

        for file in _TOKENIZER_FILES:
            src_file = os.path.join(self.model_uri, file)
            dest_file = os.path.join(output_dir, file)
            if os.path.exists(src_file):
                shutil.copy(src_file, dest_file)
                logger.info(f"✓ Copied {file} to {output_dir}")
                return output_dir
            else:
                logger.warning(f"File {file} not found in PVC path: {self.model_uri}")
                return None
            
      
    def modelscope_download(modelroute: str, revision: str | None = None ) -> Tokenizer:
        snapshot_path = snapshot_download(
            repo_id=modelroute,
            revision=revision,
            allow_patterns=_TOKENIZER_FILES,
            )
        
        tokenizer = Tokenizer.from_pretrained(snapshot_path)
        return tokenizer

'''
def download_tokenizer(model_uri: str, revision: str | None = None) -> Tokenizer:
    if model_uri.startswith("s3://"):
        return _downloader.download_tokenizer_from_s3(s3_info=model_uri, s3_route=model_uri)
    elif model_uri.startswith("pvc://"):
        pvc_path = model_uri.removeprefix("pvc://")
        return _downloader.download_tokenizer_from_pvc(pvc_path)
    elif model_uri.startswith("ms://"):
        modelroute = model_uri.removeprefix("ms://")
        return _downloader.download_tokenizer_from_modelscope(modelroute, revision)
    else:
        modelroute = model_uri.removeprefix("hf://") if model_uri.startswith("hf://") else model_uri
        return _downloader.download_tokenizer_from_huggingface(modelroute, revision)

'''