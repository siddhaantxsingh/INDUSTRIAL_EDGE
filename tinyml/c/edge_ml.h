/* Edge fault classifier: fixed-point features + INT8 MLP.
 * Dependency-free C99. Bit-exact with tinyml/edgeml/ref.py (the spec) and
 * internal/tinyml (Go). Not thread-safe: uses one static work area. */
#ifndef EDGE_ML_H
#define EDGE_ML_H
#include <stdint.h>

#define EDGE_N 1024
#define EDGE_NFEATURES 22
#define EDGE_NCLASSES 6

/* x: 1024 accelerometer samples (0.5 mg/LSB, 5120 Hz); rpm: motor speed;
 * cur: RMS motor current in 0.01 A. Writes 22 Q8 log2 features. */
void edge_features(const int16_t *x, uint16_t rpm, uint16_t cur, int32_t *feat);
/* Per-feature affine quantization of the features to int8. */
void edge_quantize(const int32_t *feat, int8_t *q);
/* INT8 MLP 22-32-16-6; writes int8 logits. */
void edge_mlp(const int8_t *q, int8_t *logits);
/* Index of the first maximum logit. */
int edge_argmax(const int8_t *logits);
/* Full pipeline; returns the class and fills feat/q/logits if non-NULL. */
int edge_classify(const int16_t *x, uint16_t rpm, uint16_t cur, int32_t *feat, int8_t *q, int8_t *logits);
#endif
