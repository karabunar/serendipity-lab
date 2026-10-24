"""Embedder: model wrapper that turns text into an L2-normalized vector.

The default model is the multilingual `intfloat/multilingual-e5-small` (384
dimensions, supports Turkish). The model is loaded when `load()` is called,
NOT when this file is imported; `sentence_transformers` is imported only then
too. So reading the package, running tests or installing the service downloads
nothing — the download happens into the HuggingFace cache at the first
`load()` (when the service starts).
"""
from __future__ import annotations

from typing import Protocol, Sequence

DEFAULT_MODEL = "intfloat/multilingual-e5-small"

# The EmbedKind constants have the same values as in embedding.proto.
KIND_PASSAGE = 0
KIND_QUERY = 1


class Embedder(Protocol):
    model_name: str

    @property
    def ready(self) -> bool: ...

    @property
    def dim(self) -> int: ...

    def encode(self, texts: Sequence[str], kind: int = KIND_PASSAGE) -> list[list[float]]: ...


class E5Embedder:
    """sentence-transformers based embedder.

    The e5 family expects the prefix "query: " for queries and "passage: " for
    documents; if the model name contains "e5" the prefix is added
    automatically, and not for other models.
    """

    def __init__(
        self,
        model_name: str = DEFAULT_MODEL,
        device: str = "cpu",
        batch_size: int = 32,
    ) -> None:
        if batch_size <= 0:
            raise ValueError("batch_size must be positive")
        self.model_name = model_name
        self._device = device
        self._batch_size = batch_size
        self._model = None
        self._dim = 0
        self._prefixed = "e5" in model_name.lower()

    @property
    def ready(self) -> bool:
        return self._model is not None

    @property
    def dim(self) -> int:
        return self._dim

    def load(self) -> None:
        """Loads the model (downloads if needed). A second call is a no-op."""
        if self._model is not None:
            return
        # Lazy import: only when load() is called (nothing is downloaded at import).
        from sentence_transformers import SentenceTransformer

        model = SentenceTransformer(self.model_name, device=self._device)
        # Renamed in sentence-transformers 5.x; keep working with 3.x/4.x.
        get_dim = getattr(model, "get_embedding_dimension", None) or model.get_sentence_embedding_dimension
        dim = get_dim()
        if not dim or dim <= 0:
            raise RuntimeError(f"could not read model dimension: {self.model_name}")
        self._model = model
        self._dim = int(dim)

    def _prefix(self, text: str, kind: int) -> str:
        if not self._prefixed:
            return text
        return ("query: " if kind == KIND_QUERY else "passage: ") + text

    def encode(self, texts: Sequence[str], kind: int = KIND_PASSAGE) -> list[list[float]]:
        if self._model is None:
            raise RuntimeError("model not loaded; call load() first")
        if not texts:
            return []
        prepared = [self._prefix(t, kind) for t in texts]
        vectors = self._model.encode(
            prepared,
            batch_size=self._batch_size,
            normalize_embeddings=True,
            convert_to_numpy=True,
            show_progress_bar=False,
        )
        return [[float(x) for x in row] for row in vectors]
