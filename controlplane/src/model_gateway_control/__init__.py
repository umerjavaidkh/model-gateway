"""Control plane for the Model Gateway.

The control plane owns the component registry, the identity graph, policy
authoring and budget definitions. It compiles all of it into immutable,
versioned snapshot layers and publishes them to the data plane.

It is never in the request path. That is the property the whole design rests on:
a control-plane outage degrades to "configuration is frozen" while traffic keeps
flowing, which is why this can run at lower availability than the Go data plane
and be written in Python without anyone noticing.
"""

__all__ = ["__version__"]

__version__ = "0.1.0"
