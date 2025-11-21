export interface ConditionUI {
  id: string
  field: string
  operator: string
  value: string
}

export interface FilterGroupUI {
  id: string
  parts: (ConditionUI | FilterGroupUI)[]
  logicalOperator: 'AND' | 'OR'
  isNested: boolean
}