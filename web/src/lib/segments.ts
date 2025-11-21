import type { Condition, SegmentFilter } from '@buf/pushpa_cotton.bufbuild_es/segments/v1/segments_pb'
import { create } from '@bufbuild/protobuf'
import { ConditionSchema, FilterPartSchema, SegmentFilterSchema } from '@buf/pushpa_cotton.bufbuild_es/segments/v1/segments_pb'
import type { ConditionUI, FilterGroupUI } from '@/pages/segments/segments'

export function generateId(): string {
  return Math.random().toString(36).substring(2, 9)
}

export function convertToPB(group: FilterGroupUI): SegmentFilter {
  const parts = group.parts.map(part => {
    if ('isNested' in part && part.isNested) {
      // It's a sub-group
      const subFilter = convertToPB(part as FilterGroupUI)
      const filterPart = create(FilterPartSchema)
      filterPart.part = { case: 'subFilter', value: subFilter }
      return filterPart
    } else {
      // It's a condition
      const condition = create(ConditionSchema, {
        field: (part as ConditionUI).field,
        operator: (part as ConditionUI).operator,
        value: (part as ConditionUI).value
      })
      const filterPart = create(FilterPartSchema)
      filterPart.part = { case: 'condition', value: condition }
      return filterPart
    }
  })

  return create(SegmentFilterSchema, {
    parts: parts,
    logicalOperator: group.logicalOperator
  })
}

// Helper function to convert protobuf SegmentFilter to UI structure
export function pbToUI(filter: SegmentFilter): FilterGroupUI {
  const parts = filter.parts?.map(part => {
    // Handle the oneof field properly by checking if condition is set
    if (part.part.case === 'condition' && part.part.value) {
      // It's a condition
      const condition = part.part.value as Condition;
      return {
        id: generateId(),
        field: condition.field,
        operator: condition.operator,
        value: condition.value
      } as ConditionUI
    } else if (part.part.case === 'subFilter' && part.part.value) {
      // It's a sub-filter
      const subFilter = part.part.value as SegmentFilter;
      return pbToUI(subFilter) // Recursive call for nested structure
    }
    return null
  }).filter(Boolean) as (ConditionUI | FilterGroupUI)[]

  return {
    id: generateId(),
    parts,
    logicalOperator: (filter.logicalOperator as 'AND' | 'OR') || 'AND',
    isNested: false
  }
}

// Helper function to convert UI structure to protobuf
export function uiToPB(group: FilterGroupUI): SegmentFilter {
  const parts = group.parts.map(part => {
    if ('isNested' in part && part.isNested) {
      // It's a sub-group
      const subFilter = uiToPB(part as FilterGroupUI)
      const filterPart = create(FilterPartSchema)
      filterPart.part = { case: 'subFilter', value: subFilter }
      return filterPart
    } else {
      // It's a condition
      const condition = create(ConditionSchema, {
        field: (part as ConditionUI).field,
        operator: (part as ConditionUI).operator,
        value: (part as ConditionUI).value
      })
      const filterPart = create(FilterPartSchema)
      filterPart.part = { case: 'condition', value: condition }
      return filterPart
    }
  })

  return create(SegmentFilterSchema, {
    parts: parts,
    logicalOperator: group.logicalOperator
  })
}

// Common functions for managing the group structure
export function addConditionToGroup(groupId: string, condition: ConditionUI, group: FilterGroupUI): FilterGroupUI {
  const updateGroup = (currentGroup: FilterGroupUI): FilterGroupUI => {
    if (currentGroup.id === groupId) {
      return {
        ...currentGroup,
        parts: [...currentGroup.parts, condition]
      }
    }

    return {
      ...currentGroup,
      parts: currentGroup.parts.map(part =>
        'isNested' in part ? updateGroup(part as FilterGroupUI) : part
      )
    }
  }

  return updateGroup(group)
}

export function addSubGroupToGroup(groupId: string, subGroup: FilterGroupUI, group: FilterGroupUI): FilterGroupUI {
  const updateGroup = (currentGroup: FilterGroupUI): FilterGroupUI => {
    if (currentGroup.id === groupId) {
      return {
        ...currentGroup,
        parts: [...currentGroup.parts, subGroup]
      }
    }

    return {
      ...currentGroup,
      parts: currentGroup.parts.map(part =>
        'isNested' in part ? updateGroup(part as FilterGroupUI) : part
      )
    }
  }

  return updateGroup(group)
}

export function removePartFromGroup(groupId: string, partId: string, group: FilterGroupUI): FilterGroupUI {
  const updateGroup = (currentGroup: FilterGroupUI): FilterGroupUI => {
    if (currentGroup.id === groupId) {
      return {
        ...currentGroup,
        parts: currentGroup.parts.filter(part =>
          ('id' in part && part.id !== partId)
        )
      }
    }

    return {
      ...currentGroup,
      parts: currentGroup.parts.map(part =>
        'isNested' in part ? updateGroup(part as FilterGroupUI) : part
      )
    }
  }

  return updateGroup(group)
}

export function updateGroupOperator(groupId: string, operator: 'AND' | 'OR', group: FilterGroupUI): FilterGroupUI {
  const updateGroup = (currentGroup: FilterGroupUI): FilterGroupUI => {
    if (currentGroup.id === groupId) {
      return {
        ...currentGroup,
        logicalOperator: operator
      }
    }

    return {
      ...currentGroup,
      parts: currentGroup.parts.map(part =>
        'isNested' in part ? updateGroup(part as FilterGroupUI) : part
      )
    }
  }

  return updateGroup(group)
}