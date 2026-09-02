# A component image, as a publisher would ship one.
#
# It exists so the admission path can be exercised against a real container
# runtime with the real isolation flags — the stub runtime beside it checks the
# wiring on a machine with no Docker, and this checks that the flags the
# sandbox passes actually let a well-behaved component work.
FROM python:3.12-alpine

# The sandbox runs the container as nobody with no capabilities, so nothing here
# may need root at runtime.
COPY stub-component.py /app/stub-component.py

# No network is available inside the sandbox, so everything the component needs
# must already be in the image. This one needs only the standard library.
# Baked in rather than passed at run time: the sandbox deliberately passes no
# caller-controlled environment into the container, so "a component that
# misbehaves" has to be a different image — which is what it would be in
# reality too.
ARG BEHAVIOUR=conforming
ENV STUB_BEHAVIOUR=${BEHAVIOUR}

USER 65534:65534
ENTRYPOINT ["python3", "/app/stub-component.py"]
